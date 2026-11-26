// Offline LoRa mesh radio page (/mesh), message persistence, and serial/USB device status APIs.
package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MeshMessageView represents a persisted LoRa / Meshtastic / MeshCore message.
type MeshMessageView struct {
	ID         string  `json:"id"`
	Protocol   string  `json:"protocol"`
	Direction  string  `json:"direction"` // "rx" or "tx"
	Channel    string  `json:"channel"`
	FromID     string  `json:"from_id"`
	FromName   string  `json:"from_name"`
	ToID       string  `json:"to_id"`
	Body       string  `json:"body"`
	SNR        float64 `json:"snr"`
	RSSI       int     `json:"rssi"`
	CreatedAt  string  `json:"created_at"`
}

// MeshNodeView represents a discovered LoRa / Meshtastic / MeshCore radio node.
type MeshNodeView struct {
	NodeID    string  `json:"node_id"`
	ShortName string  `json:"short_name"`
	LongName  string  `json:"long_name"`
	Hardware  string  `json:"hardware"`
	Protocol  string  `json:"protocol"`
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
	Battery   int     `json:"battery"`
	SNR       float64 `json:"snr"`
	RSSI      int     `json:"rssi"`
	LastHeard string  `json:"last_heard"`
}

func (s *Server) ensureMeshTables() {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS mesh_messages (
			id TEXT PRIMARY KEY,
			protocol TEXT NOT NULL DEFAULT 'meshtastic',
			direction TEXT NOT NULL DEFAULT 'rx',
			channel TEXT NOT NULL DEFAULT '0',
			from_id TEXT NOT NULL DEFAULT '',
			from_name TEXT NOT NULL DEFAULT '',
			to_id TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL DEFAULT '',
			snr REAL NOT NULL DEFAULT 0,
			rssi INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_mesh_messages_created ON mesh_messages(created_at DESC);
		CREATE TABLE IF NOT EXISTS mesh_nodes (
			node_id TEXT PRIMARY KEY,
			short_name TEXT NOT NULL DEFAULT '',
			long_name TEXT NOT NULL DEFAULT '',
			hardware TEXT NOT NULL DEFAULT '',
			protocol TEXT NOT NULL DEFAULT 'meshtastic',
			lat REAL NOT NULL DEFAULT 0,
			lng REAL NOT NULL DEFAULT 0,
			battery INTEGER NOT NULL DEFAULT -1,
			snr REAL NOT NULL DEFAULT 0,
			rssi INTEGER NOT NULL DEFAULT 0,
			last_heard DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
}

func (s *Server) listMeshMessages(limit int) []MeshMessageView {
	if s.db == nil {
		return []MeshMessageView{}
	}
	if limit <= 0 || limit > 300 {
		limit = 150
	}
	s.ensureMeshTables()
	rows, err := s.db.Query(`
		SELECT id, protocol, direction, channel, from_id, from_name, to_id, body, snr, rssi, created_at
		FROM mesh_messages
		ORDER BY created_at DESC, rowid DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return []MeshMessageView{}
	}
	defer rows.Close()
	out := make([]MeshMessageView, 0)
	for rows.Next() {
		var m MeshMessageView
		if err := rows.Scan(&m.ID, &m.Protocol, &m.Direction, &m.Channel, &m.FromID, &m.FromName, &m.ToID, &m.Body, &m.SNR, &m.RSSI, &m.CreatedAt); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) listMeshNodes() []MeshNodeView {
	if s.db == nil {
		return []MeshNodeView{}
	}
	s.ensureMeshTables()
	rows, err := s.db.Query(`
		SELECT node_id, short_name, long_name, hardware, protocol, lat, lng, battery, snr, rssi, last_heard
		FROM mesh_nodes
		ORDER BY last_heard DESC
		LIMIT 200
	`)
	if err != nil {
		return []MeshNodeView{}
	}
	defer rows.Close()
	out := make([]MeshNodeView, 0)
	for rows.Next() {
		var n MeshNodeView
		if err := rows.Scan(&n.NodeID, &n.ShortName, &n.LongName, &n.Hardware, &n.Protocol, &n.Lat, &n.Lng, &n.Battery, &n.SNR, &n.RSSI, &n.LastHeard); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) handleMeshPage(w http.ResponseWriter, r *http.Request) {
	s.ensureMeshTables()
	data := PageData{
		Title:        "Meshtastic / LoRa Bridge",
		ActiveNav:    "apps",
		SessionToken: s.sessionStart.Format(time.RFC3339),
	}
	s.render(w, r, "mesh.html", data)
}

func (s *Server) handleAPIMeshState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"messages": s.listMeshMessages(150),
		"nodes":    s.listMeshNodes(),
	})
}

func (s *Server) handleAPIMeshMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.ensureMeshTables()
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "message body is required"})
		return
	}
	if len(body) > 2000 {
		body = body[:2000]
	}
	protocol := strings.TrimSpace(r.FormValue("protocol"))
	if protocol == "" {
		protocol = "meshtastic"
	}
	direction := strings.TrimSpace(r.FormValue("direction"))
	if direction != "tx" && direction != "rx" {
		direction = "rx"
	}
	channel := strings.TrimSpace(r.FormValue("channel"))
	if channel == "" {
		channel = "0"
	}
	fromID := strings.TrimSpace(r.FormValue("from_id"))
	fromName := strings.TrimSpace(r.FormValue("from_name"))
	toID := strings.TrimSpace(r.FormValue("to_id"))
	snr, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("snr")), 64)
	rssi, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("rssi")))

	id := fmt.Sprintf("mesh-%d", time.Now().UnixNano())
	now := time.Now().Format("2006-01-02 15:04:05")
	if s.db != nil {
		_, err := s.db.Exec(`
			INSERT INTO mesh_messages (id, protocol, direction, channel, from_id, from_name, to_id, body, snr, rssi, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, id, protocol, direction, channel, fromID, fromName, toID, body, snr, rssi, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"message": MeshMessageView{
			ID:        id,
			Protocol:  protocol,
			Direction: direction,
			Channel:   channel,
			FromID:    fromID,
			FromName:  fromName,
			ToID:      toID,
			Body:      body,
			SNR:       snr,
			RSSI:      rssi,
			CreatedAt: now,
		},
	})
}

func (s *Server) handleAPIMeshNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.ensureMeshTables()
	nodeID := strings.TrimSpace(r.FormValue("node_id"))
	if nodeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "node_id is required"})
		return
	}
	shortName := strings.TrimSpace(r.FormValue("short_name"))
	longName := strings.TrimSpace(r.FormValue("long_name"))
	hardware := strings.TrimSpace(r.FormValue("hardware"))
	protocol := strings.TrimSpace(r.FormValue("protocol"))
	if protocol == "" {
		protocol = "meshtastic"
	}
	lat, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("lat")), 64)
	lng, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("lng")), 64)
	battery := -1
	if bs := strings.TrimSpace(r.FormValue("battery")); bs != "" {
		if v, err := strconv.Atoi(bs); err == nil {
			battery = v
		}
	}
	snr, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("snr")), 64)
	rssi, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("rssi")))
	now := time.Now().Format("2006-01-02 15:04:05")

	if s.db != nil {
		_, err := s.db.Exec(`
			INSERT INTO mesh_nodes (node_id, short_name, long_name, hardware, protocol, lat, lng, battery, snr, rssi, last_heard)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(node_id) DO UPDATE SET
				short_name = CASE WHEN excluded.short_name != '' THEN excluded.short_name ELSE mesh_nodes.short_name END,
				long_name  = CASE WHEN excluded.long_name  != '' THEN excluded.long_name  ELSE mesh_nodes.long_name  END,
				hardware   = CASE WHEN excluded.hardware   != '' THEN excluded.hardware   ELSE mesh_nodes.hardware   END,
				protocol   = excluded.protocol,
				lat        = CASE WHEN excluded.lat != 0 THEN excluded.lat ELSE mesh_nodes.lat END,
				lng        = CASE WHEN excluded.lng != 0 THEN excluded.lng ELSE mesh_nodes.lng END,
				battery    = CASE WHEN excluded.battery >= 0 THEN excluded.battery ELSE mesh_nodes.battery END,
				snr        = CASE WHEN excluded.snr != 0 THEN excluded.snr ELSE mesh_nodes.snr END,
				rssi       = CASE WHEN excluded.rssi != 0 THEN excluded.rssi ELSE mesh_nodes.rssi END,
				last_heard = excluded.last_heard
		`, nodeID, shortName, longName, hardware, protocol, lat, lng, battery, snr, rssi, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAPIMeshClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.ensureMeshTables()
	if s.db != nil {
		_, _ = s.db.Exec(`DELETE FROM mesh_messages`)
		_, _ = s.db.Exec(`DELETE FROM mesh_nodes`)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
