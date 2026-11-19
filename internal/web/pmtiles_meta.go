package web

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
