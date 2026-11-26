// PMTiles v3 metadata parsing and native HTTP Z/X/Y vector/raster tile serving.
package web

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
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

const pmtilesHeaderLen = 127

// pmTilesMeta holds header bounds/zoom metadata parsed natively from a PMTiles v3 file.
type pmTilesMeta struct {
	Bounds       [4]float64 `json:"bounds"`
	Center       [3]float64 `json:"center"`
	MinZoom      int        `json:"minzoom"`
	MaxZoom      int        `json:"maxzoom"`
	TileType     string     `json:"tiletype"`
	Name         string     `json:"name,omitempty"`
	Description  string     `json:"description,omitempty"`
	Attribution  string     `json:"attribution,omitempty"`
	VectorLayers any        `json:"vector_layers,omitempty"`
}

func tileTypeString(code byte) string {
	switch code {
	case 1:
		return "mvt"
	case 2:
		return "png"
	case 3:
		return "jpeg"
	case 4:
		return "webp"
	case 5:
		return "avif"
	default:
		return "unknown"
	}
}

// ParsePMTilesMeta reads the 127-byte PMTiles v3 binary header and optional JSON metadata.
func ParsePMTilesMeta(path string) (*pmTilesMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var hdr [pmtilesHeaderLen]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, fmt.Errorf("read pmtiles header: %w", err)
	}
	if string(hdr[0:7]) != "PMTiles" || hdr[7] != 3 {
		return nil, fmt.Errorf("unsupported pmtiles header/version")
	}

	readI32 := func(off int) int32 {
		return int32(binary.LittleEndian.Uint32(hdr[off : off+4]))
	}

	minLng := float64(readI32(102)) / 1e7
	minLat := float64(readI32(106)) / 1e7
	maxLng := float64(readI32(110)) / 1e7
	maxLat := float64(readI32(114)) / 1e7

	centerZoom := int(hdr[118])
	centerLng := float64(readI32(119)) / 1e7
	centerLat := float64(readI32(123)) / 1e7
	if centerLng == 0 && centerLat == 0 && (minLng != 0 || maxLng != 0 || minLat != 0 || maxLat != 0) {
		centerLng = (minLng + maxLng) / 2
		centerLat = (minLat + maxLat) / 2
	}

	meta := &pmTilesMeta{
		Bounds:   [4]float64{minLng, minLat, maxLng, maxLat},
		Center:   [3]float64{centerLng, centerLat, float64(centerZoom)},
		MinZoom:  int(hdr[100]),
		MaxZoom:  int(hdr[101]),
		TileType: tileTypeString(hdr[99]),
	}

	jsonOff := binary.LittleEndian.Uint64(hdr[24:32])
	jsonLen := binary.LittleEndian.Uint64(hdr[32:40])
	const maxMetaBytes = 2 << 20 // 2 MiB guard
	if jsonLen > 0 && jsonLen <= maxMetaBytes {
		if _, err := f.Seek(int64(jsonOff), io.SeekStart); err == nil {
			raw := make([]byte, jsonLen)
			if _, err := io.ReadFull(f, raw); err == nil {
				// internal_compression == 2 indicates gzip (or check magic bytes 0x1f 0x8b)
				if hdr[97] == 2 || (len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b) {
					if gz, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
						if unz, err := io.ReadAll(io.LimitReader(gz, maxMetaBytes)); err == nil {
							raw = unz
						}
						_ = gz.Close()
					}
				}
				var jm struct {
					Name         string `json:"name"`
					Description  string `json:"description"`
					Attribution  string `json:"attribution"`
					VectorLayers any    `json:"vector_layers"`
				}
				if json.Unmarshal(raw, &jm) == nil {
					meta.Name = jm.Name
					meta.Description = jm.Description
					meta.Attribution = jm.Attribution
					meta.VectorLayers = jm.VectorLayers
				}
			}
		}
	}

	return meta, nil
}

func formatMapArchiveTitle(stem string) string {
	clean := strings.ReplaceAll(strings.ReplaceAll(stem, "_", " "), "-", " ")
	parts := strings.Fields(clean)
	// Strip trailing YYYY MM build date tokens (e.g. "2025 12") for clean place labels
	if len(parts) >= 3 && len(parts[len(parts)-2]) == 4 && len(parts[len(parts)-1]) == 2 {
		if parts[len(parts)-2][0] == '2' && parts[len(parts)-1][0] >= '0' && parts[len(parts)-1][0] <= '1' {
			parts = parts[:len(parts)-2]
		}
	}
	for i, p := range parts {
		if len(p) > 0 && p[0] >= 'a' && p[0] <= 'z' {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	if len(parts) == 0 {
		return stem
	}
	return strings.Join(parts, " ")
}

// handleMapsFiles serves raw .pmtiles archives from data/maps/ with HTTP Range support.
func (s *Server) handleMapsFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/maps/files/")
	if rel == "" || strings.Contains(rel, "/") || strings.Contains(rel, "\\") || strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	if !strings.HasSuffix(strings.ToLower(rel), ".pmtiles") {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join(s.dataDir, "maps", rel)
	f, err := os.Open(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.pmtiles")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// handleAPIMapsTileJSON returns parsed TileJSON/PMTiles v3 metadata directly from disk.
func (s *Server) handleAPIMapsTileJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	name = strings.TrimSuffix(name, ".pmtiles")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		writeJSONError(w, http.StatusBadRequest, "invalid map archive name")
		return
	}
	path := filepath.Join(s.dataDir, "maps", name+".pmtiles")
	if _, err := os.Stat(path); err != nil {
		// Case-insensitive lookup fallback
		entries, _ := os.ReadDir(filepath.Join(s.dataDir, "maps"))
		found := false
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(e.Name(), name+".pmtiles") {
				path = filepath.Join(s.dataDir, "maps", e.Name())
				found = true
				break
			}
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "map archive not found")
			return
		}
	}

	meta, err := ParsePMTilesMeta(path)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{
		"tilejson":      "3.0.0",
		"file":          filepath.Base(path),
		"pmtiles_url":   "/maps/files/" + filepath.Base(path),
		"bounds":        meta.Bounds,
		"center":        meta.Center,
		"minzoom":       meta.MinZoom,
		"maxzoom":       meta.MaxZoom,
		"tiletype":      meta.TileType,
		"name":          meta.Name,
		"description":   meta.Description,
		"attribution":   meta.Attribution,
		"vector_layers": meta.VectorLayers,
	}
	writeJSON(w, http.StatusOK, resp)
}
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
