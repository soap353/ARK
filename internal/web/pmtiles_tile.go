package web

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type pmDirEntry struct {
	TileID    uint64
	Offset    uint64
	Length    uint32
	RunLength uint32
}

type pmArchiveIndex struct {
	path            string
	modTime         time.Time
	size            int64
	rootOffset      uint64
	rootLength      uint64
	leafOffset      uint64
	tileDataOffset  uint64
	internalComp    byte
	tileComp        byte
	tileType        byte
	minZoom         uint8
	maxZoom         uint8
	bounds          [4]float64 // minLng, minLat, maxLng, maxLat
	rootEntries     []pmDirEntry
}

var (
	pmIndexCacheMu sync.RWMutex
	pmIndexCache   = make(map[string]*pmArchiveIndex)
)

// zxyToTileID converts Z/X/Y slippy tile coordinates to a PMTiles v3 Hilbert TileID.
func zxyToTileID(z uint8, x, y uint32) uint64 {
	if z == 0 {
		return 0
	}
	n := uint64(1) << uint(z)
	acc := (n*n - 1) / 3
	tx, ty := uint64(x), uint64(y)
	var d uint64
	for s := n >> 1; s > 0; s >>= 1 {
		var rx, ry uint64
		if (tx & s) > 0 {
			rx = 1
		}
		if (ty & s) > 0 {
			ry = 1
		}
		d += s * s * ((3 * rx) ^ ry)
		if ry == 0 {
			if rx == 1 {
				tx = n - 1 - tx
				ty = n - 1 - ty
			}
			tx, ty = ty, tx
		}
	}
	return acc + d
}

func decodePMDirectory(raw []byte, internalComp byte) ([]pmDirEntry, error) {
	data := raw
	if internalComp == 2 || (len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b) {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		unz, err := io.ReadAll(io.LimitReader(gz, 8<<20))
		_ = gz.Close()
		if err != nil {
			return nil, err
		}
		data = unz
	}
	r := bytes.NewReader(data)
	numEntries, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if numEntries > 1_000_000 {
		return nil, fmt.Errorf("pmtiles directory too large: %d", numEntries)
	}
	entries := make([]pmDirEntry, numEntries)
	var lastID uint64
	for i := range entries {
		d, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		lastID += d
		entries[i].TileID = lastID
	}
	for i := range entries {
		rl, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		entries[i].RunLength = uint32(rl)
	}
	for i := range entries {
		l, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		entries[i].Length = uint32(l)
	}
	for i := range entries {
		off, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		if off == 0 && i > 0 {
			entries[i].Offset = entries[i-1].Offset + uint64(entries[i-1].Length)
		} else if off > 0 {
			entries[i].Offset = off - 1
		}
	}
	return entries, nil
}

func findPMEntry(entries []pmDirEntry, tileID uint64) (pmDirEntry, bool) {
	if len(entries) == 0 {
		return pmDirEntry{}, false
	}
	idx := sort.Search(len(entries), func(i int) bool {
		return entries[i].TileID > tileID
	}) - 1
	if idx < 0 {
		return pmDirEntry{}, false
	}
	e := entries[idx]
	if e.RunLength == 0 {
		// Leaf directory entry
		return e, true
	}
	if tileID >= e.TileID && tileID < e.TileID+uint64(e.RunLength) {
		return e, true
	}
	return pmDirEntry{}, false
}

func loadPMArchiveIndex(path string) (*pmArchiveIndex, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	pmIndexCacheMu.RLock()
	cached := pmIndexCache[path]
	pmIndexCacheMu.RUnlock()
	if cached != nil && cached.modTime.Equal(info.ModTime()) && cached.size == info.Size() {
		return cached, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var hdr [pmtilesHeaderLen]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, err
	}
	if string(hdr[0:7]) != "PMTiles" || hdr[7] != 3 {
		return nil, fmt.Errorf("unsupported pmtiles version")
	}

	readI32 := func(off int) int32 {
		return int32(binary.LittleEndian.Uint32(hdr[off : off+4]))
	}
	idx := &pmArchiveIndex{
		path:           path,
		modTime:        info.ModTime(),
		size:           info.Size(),
		rootOffset:     binary.LittleEndian.Uint64(hdr[8:16]),
		rootLength:     binary.LittleEndian.Uint64(hdr[16:24]),
		leafOffset:     binary.LittleEndian.Uint64(hdr[40:48]),
		tileDataOffset: binary.LittleEndian.Uint64(hdr[56:64]),
		internalComp:   hdr[97],
		tileComp:       hdr[98],
		tileType:       hdr[99],
		minZoom:        hdr[100],
		maxZoom:        hdr[101],
		bounds: [4]float64{
			float64(readI32(102)) / 1e7,
			float64(readI32(106)) / 1e7,
			float64(readI32(110)) / 1e7,
			float64(readI32(114)) / 1e7,
		},
	}

	if idx.rootLength > 0 && idx.rootLength <= (4<<20) {
		rootBytes := make([]byte, idx.rootLength)
		if _, err := f.ReadAt(rootBytes, int64(idx.rootOffset)); err != nil {
			return nil, err
		}
		entries, err := decodePMDirectory(rootBytes, idx.internalComp)
		if err != nil {
			return nil, err
		}
		idx.rootEntries = entries
	}

	pmIndexCacheMu.Lock()
	if len(pmIndexCache) >= 64 {
		pmIndexCache = make(map[string]*pmArchiveIndex)
	}
	pmIndexCache[path] = idx
	pmIndexCacheMu.Unlock()
	return idx, nil
}

func (idx *pmArchiveIndex) readRawTile(z uint8, x, y uint32) ([]byte, bool) {
	if z < idx.minZoom || z > idx.maxZoom || idx.tileType != 1 {
		return nil, false
	}
	tileID := zxyToTileID(z, x, y)
	entry, ok := findPMEntry(idx.rootEntries, tileID)
	if !ok {
		return nil, false
	}

	f, err := os.Open(idx.path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	// Follow up to 3 leaf directory hops without retaining leaf directories in RAM
	for depth := 0; entry.RunLength == 0 && depth < 3; depth++ {
		if entry.Length == 0 || entry.Length > (4<<20) {
			return nil, false
		}
		leafBuf := make([]byte, entry.Length)
		if _, err := f.ReadAt(leafBuf, int64(idx.leafOffset+entry.Offset)); err != nil {
			return nil, false
		}
		leafEntries, err := decodePMDirectory(leafBuf, idx.internalComp)
		if err != nil {
			return nil, false
		}
		entry, ok = findPMEntry(leafEntries, tileID)
		if !ok {
			return nil, false
		}
	}

	if entry.RunLength == 0 || entry.Length == 0 || entry.Length > (8<<20) {
		return nil, false
	}
	tileBuf := make([]byte, entry.Length)
	if _, err := f.ReadAt(tileBuf, int64(idx.tileDataOffset+entry.Offset)); err != nil {
		return nil, false
	}
	return tileBuf, true
}

func gunzipTileBytes(b []byte, comp byte) ([]byte, bool) {
	if comp == 2 || (len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b) {
		gz, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, false
		}
		out, err := io.ReadAll(io.LimitReader(gz, 8<<20))
		_ = gz.Close()
		if err != nil {
			return nil, false
		}
		return out, true
	}
	return b, true
}

func tileLatLngBounds(z uint8, x, y uint32) (minLng, minLat, maxLng, maxLat float64) {
	n := math.Exp2(float64(z))
	minLng = float64(x)/n*360.0 - 180.0
	maxLng = float64(x+1)/n*360.0 - 180.0
	yToLat := func(ty uint32) float64 {
		t := math.Pi * (1.0 - 2.0*float64(ty)/n)
		return math.Atan(math.Sinh(t)) * 180.0 / math.Pi
	}
	maxLat = yToLat(y)
	minLat = yToLat(y + 1)
	return
}

func boundsOverlap(a [4]float64, minLng, minLat, maxLng, maxLat float64) bool {
	if a == [4]float64{} {
		return true
	}
	return a[0] <= maxLng && a[2] >= minLng && a[1] <= maxLat && a[3] >= minLat
}

// handleUnifiedMapTiles serves /maps/tiles/{z}/{x}/{y}.mvt by seamlessly streaming and combining
// matching tiles across all installed .pmtiles files in data/maps/ with O(1) memory per request.
func (s *Server) handleUnifiedMapTiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/maps/tiles/")
	rel = strings.TrimSuffix(rel, ".mvt")
	rel = strings.TrimSuffix(rel, ".pbf")
	parts := strings.Split(rel, "/")
	if len(parts) != 3 {
		http.NotFound(w, r)
		return
	}
	zi, err1 := strconv.Atoi(parts[0])
	xi, err2 := strconv.Atoi(parts[1])
	yi, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || zi < 0 || zi > 22 || xi < 0 || yi < 0 {
		http.NotFound(w, r)
		return
	}
	z := uint8(zi)
	x := uint32(xi)
	y := uint32(yi)

	mapsDir := filepath.Join(s.dataDir, "maps")
	entries, err := os.ReadDir(mapsDir)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	minLng, minLat, maxLng, maxLat := tileLatLngBounds(z, x, y)
	type matchedTile struct {
		bytes []byte
		comp  byte
	}
	var matches []matchedTile

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".pmtiles") {
			continue
		}
		idx, err := loadPMArchiveIndex(filepath.Join(mapsDir, e.Name()))
		if err != nil {
			continue
		}
		if !boundsOverlap(idx.bounds, minLng, minLat, maxLng, maxLat) {
			continue
		}
		if tileBytes, ok := idx.readRawTile(z, x, y); ok && len(tileBytes) > 0 {
			matches = append(matches, matchedTile{bytes: tileBytes, comp: idx.tileComp})
		}
	}

	if len(matches) == 0 {
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.mapbox-vector-tile")
	w.Header().Set("Cache-Control", "no-cache")

	// Single archive match: stream compressed bytes directly with zero recompression
	if len(matches) == 1 {
		m := matches[0]
		if m.comp == 2 || (len(m.bytes) >= 2 && m.bytes[0] == 0x1f && m.bytes[1] == 0x8b) {
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(m.bytes)
		return
	}

	// Overlapping border/overview tile: concatenate uncompressed MVT Layer messages.
	// Return uncompressed (or gzipped) merged MVT protobuf so all installed regions appear simultaneously.
	var merged bytes.Buffer
	for _, m := range matches {
		if raw, ok := gunzipTileBytes(m.bytes, m.comp); ok {
			merged.Write(raw)
		}
	}
	var gzOut bytes.Buffer
	zw := gzip.NewWriter(&gzOut)
	_, _ = zw.Write(merged.Bytes())
	_ = zw.Close()

	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(gzOut.Bytes())
}
