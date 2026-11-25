package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ark/internal/jobs"
)

const (
	peerDiscoveryPort  = 41234
	peerBeaconMagic    = "ark-peer-v1"
	peerBeaconInterval = 5 * time.Second
	peerStaleTTL       = 20 * time.Second
)

type peerBeacon struct {
	Magic          string `json:"magic"`
	InstanceID     string `json:"id"`
	Name           string `json:"name"`
	Port           string `json:"port"`
	AllowDownloads bool   `json:"allow_downloads"`
	HTTPS          bool   `json:"https,omitempty"`
}

func lanPeerTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return tr
}

type discoveredPeerRecord struct {
	InstanceID     string
	Name           string
	BaseURL        string
	AllowDownloads bool
	LastSeen       time.Time
}

// PeerCatalogItem is an item exposed by another Ark instance over LAN.
type PeerCatalogItem struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category"`
	Size        string `json:"size"`
	SizeBytes   int64  `json:"size_bytes"`
	Date        string `json:"date"`
	RelPath     string `json:"rel_path"`
	FileName    string `json:"file_name"`
	OpenURL     string `json:"open_url"`
	IconURL     string `json:"icon_url"`
	Stem        string `json:"stem,omitempty"`
}

// PeerCatalogResponse is returned by GET /api/peers/catalog.
type PeerCatalogResponse struct {
	OK             bool              `json:"ok"`
	Discoverable   bool              `json:"discoverable"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	AllowDownloads bool              `json:"allow_downloads"`
	Items          []PeerCatalogItem `json:"items"`
}

// PeerLibraryView is returned to the Library UI for each reachable peer with content.
type PeerLibraryView struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	BaseURL        string            `json:"base_url"`
	AllowDownloads bool              `json:"allow_downloads"`
	Items          []PeerCatalogItem `json:"items"`
}

func generateInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "ark-" + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("ark-%d", time.Now().UnixNano())
}

func (s *Server) isPeerDiscoverable() bool {
	return s.isSettingEnabled("peer_discoverable", true)
}

func (s *Server) isPeerDownloadsAllowed() bool {
	return s.isSettingEnabled("peer_allow_downloads", true)
}

func (s *Server) peerDisplayName() string {
	cfg := s.fetchSettings()
	name := strings.TrimSpace(cfg["device_name"])
	if name != "" && !strings.EqualFold(name, "ark") {
		return name
	}
	if hn, err := os.Hostname(); err == nil && strings.TrimSpace(hn) != "" {
		return strings.TrimSpace(hn)
	}
	if name != "" {
		return name
	}
	return "Ark"
}

func (s *Server) listenPort() string {
	cfg := s.fetchSettings()
	if p := strings.TrimSpace(cfg["port"]); p != "" {
		return p
	}
	return "8080"
}

func (s *Server) parseManualPeerHosts() []string {
	raw := strings.TrimSpace(s.fetchSettings()["peer_manual_hosts"])
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
	var out []string
	seen := make(map[string]bool)
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.HasPrefix(f, "http://") && !strings.HasPrefix(f, "https://") {
			f = "http://" + f
		}
		f = strings.TrimRight(f, "/")
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// StartPeerDiscovery starts zero-dependency UDP LAN beacon broadcast and listening.
// shortcut: UDP broadcast is subnet-local (won't cross L3 routers); manual peer_manual_hosts in Settings covers routed/isolated subnets.
func (s *Server) StartPeerDiscovery(ctx context.Context) {
	go s.runPeerListener(ctx)
	go s.runPeerBroadcaster(ctx)
}

func (s *Server) runPeerListener(ctx context.Context) {
	pc, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", peerDiscoveryPort))
	if err != nil {
		log.Printf("[Ark] Peer UDP listener unavailable on :%d (%v); manual peers still active", peerDiscoveryPort, err)
		return
	}
	defer pc.Close()
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()

	buf := make([]byte, 2048)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		var b peerBeacon
		if err := json.Unmarshal(buf[:n], &b); err != nil {
			continue
		}
		if b.Magic != peerBeaconMagic || b.InstanceID == "" || b.InstanceID == s.instanceID {
			continue
		}
		udpAddr, ok := addr.(*net.UDPAddr)
		if !ok || udpAddr.IP == nil || udpAddr.IP.IsLoopback() {
			continue
		}
		port := strings.TrimSpace(b.Port)
		if port == "" {
			port = "8080"
		}
		scheme := "http"
		if b.HTTPS {
			scheme = "https"
		}
		baseURL := fmt.Sprintf("%s://%s:%s", scheme, udpAddr.IP.String(), port)
		name := strings.TrimSpace(b.Name)
		if name == "" {
			name = udpAddr.IP.String()
		}
		s.discoveredPeers.Store(baseURL, discoveredPeerRecord{
			InstanceID:     b.InstanceID,
			Name:           name,
			BaseURL:        baseURL,
			AllowDownloads: b.AllowDownloads,
			LastSeen:       time.Now(),
		})
		s.peerByID.Store(b.InstanceID, baseURL)
	}
}

func (s *Server) runPeerBroadcaster(ctx context.Context) {
	sendOnce := func() {
		// Prune stale UDP peers
		cutoff := time.Now().Add(-peerStaleTTL)
		s.discoveredPeers.Range(func(k, v any) bool {
			if rec, ok := v.(discoveredPeerRecord); ok && rec.LastSeen.Before(cutoff) {
				s.discoveredPeers.Delete(k)
			}
			return true
		})

		if !s.isPeerDiscoverable() {
			return
		}
		payload, err := json.Marshal(peerBeacon{
			Magic:          peerBeaconMagic,
			InstanceID:     s.instanceID,
			Name:           s.peerDisplayName(),
			Port:           s.listenPort(),
			AllowDownloads: s.isPeerDownloadsAllowed(),
			HTTPS:          s.isSettingEnabled("https_enabled", false),
		})
		if err != nil {
			return
		}
		for _, dst := range lanBroadcastAddrs(peerDiscoveryPort) {
			conn, err := net.DialUDP("udp4", nil, dst)
			if err != nil {
				continue
			}
			_, _ = conn.Write(payload)
			_ = conn.Close()
		}
	}

	sendOnce()
	ticker := time.NewTicker(peerBeaconInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendOnce()
		}
	}
}

func lanBroadcastAddrs(port int) []*net.UDPAddr {
	seen := map[string]bool{}
	var out []*net.UDPAddr
	add := func(ip net.IP) {
		ip4 := ip.To4()
		if ip4 == nil {
			return
		}
		key := ip4.String()
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, &net.UDPAddr{IP: ip4, Port: port})
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagBroadcast == 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipNet, ok := a.(*net.IPNet)
				if !ok || ipNet.IP == nil {
					continue
				}
				ip4 := ipNet.IP.To4()
				if ip4 == nil || len(ipNet.Mask) != 4 {
					continue
				}
				bcast := make(net.IP, 4)
				for i := 0; i < 4; i++ {
					bcast[i] = ip4[i] | ^ipNet.Mask[i]
				}
				add(bcast)
			}
		}
	}
	add(net.IPv4bcast)
	return out
}

func validatePeerRelPath(raw string) (subdir string, cleanRel string, ok bool) {
	cleaned := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")), "/")
	if cleaned == "" || strings.Contains(cleaned, "..") {
		return "", "", false
	}
	for _, prefix := range []string{"zim/", "media/docs/", "media/video/", "media/music/"} {
		if strings.HasPrefix(cleaned, prefix) {
			rest := strings.TrimPrefix(cleaned, prefix)
			if rest == "" || strings.Contains(rest, "/") || rest == "." || rest == ".." {
				return "", "", false
			}
			return strings.TrimSuffix(prefix, "/"), cleaned, true
		}
	}
	return "", "", false
}

func (s *Server) handleAPIPeersCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.isPeerDiscoverable() {
		writeJSON(w, http.StatusForbidden, PeerCatalogResponse{
			OK:           false,
			Discoverable: false,
		})
		return
	}

	rawItems := s.scanDiskLibraryItems("", "")
	items := make([]PeerCatalogItem, 0, len(rawItems))
	for _, it := range rawItems {
		if _, cleanRel, ok := validatePeerRelPath(it.RelPath); ok {
			var sz int64
			if st, err := os.Stat(filepath.Join(s.dataDir, filepath.FromSlash(cleanRel))); err == nil {
				sz = st.Size()
			}
			items = append(items, PeerCatalogItem{
				Title:       it.Title,
				Description: it.Description,
				Category:    it.Category,
				Size:        it.Size,
				SizeBytes:   sz,
				Date:        it.Date,
				RelPath:     cleanRel,
				FileName:    it.FileName,
				OpenURL:     it.OpenURL,
				IconURL:     it.IconURL,
				Stem:        it.Stem,
			})
		}
	}

	writeJSON(w, http.StatusOK, PeerCatalogResponse{
		OK:             true,
		Discoverable:   true,
		ID:             s.instanceID,
		Name:           s.peerDisplayName(),
		AllowDownloads: s.isPeerDownloadsAllowed(),
		Items:          items,
	})
}

func (s *Server) handleAPIPeersDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.isPeerDiscoverable() || !s.isPeerDownloadsAllowed() {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"ok":    false,
			"error": "peer downloads are disabled on this Ark instance",
		})
		return
	}
	_, cleanRel, ok := validatePeerRelPath(r.URL.Query().Get("rel"))
	if !ok {
		http.Error(w, "Invalid item path", http.StatusBadRequest)
		return
	}
	fullPath := filepath.Join(s.dataDir, filepath.FromSlash(cleanRel))
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
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(cleanRel)))
	http.ServeContent(w, r, filepath.Base(cleanRel), info.ModTime(), f)
}

func (s *Server) handleAPIPeersList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	cat := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("cat")))

	candidates := make(map[string]bool)
	cutoff := time.Now().Add(-peerStaleTTL)
	s.discoveredPeers.Range(func(k, v any) bool {
		if rec, ok := v.(discoveredPeerRecord); ok {
			if rec.LastSeen.After(cutoff) && rec.BaseURL != "" {
				candidates[rec.BaseURL] = true
			}
		}
		return true
	})
	for _, manual := range s.parseManualPeerHosts() {
		candidates[manual] = true
	}

	if len(candidates) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peers": []PeerLibraryView{}})
		return
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		peers []PeerLibraryView
	)
	client := &http.Client{Timeout: 2500 * time.Millisecond, Transport: lanPeerTransport()}

	for baseURL := range candidates {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(base, "/")+"/api/peers/catalog", nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			var catResp PeerCatalogResponse
			if err := json.NewDecoder(resp.Body).Decode(&catResp); err != nil {
				return
			}
			if !catResp.OK || !catResp.Discoverable || catResp.ID == s.instanceID || len(catResp.Items) == 0 {
				return
			}

			filtered := make([]PeerCatalogItem, 0, len(catResp.Items))
			for _, it := range catResp.Items {
				if cat != "" && cat != "all" && !strings.EqualFold(it.Category, cat) {
					continue
				}
				if q != "" && !strings.Contains(strings.ToLower(it.Title+" "+it.Description+" "+it.FileName+" "+it.Category), q) {
					continue
				}
				filtered = append(filtered, it)
			}
			if len(filtered) == 0 {
				return
			}
			name := strings.TrimSpace(catResp.Name)
			if name == "" {
				name = base
			}
			normBase := strings.TrimRight(base, "/")
			s.peerByID.Store(catResp.ID, normBase)
			mu.Lock()
			peers = append(peers, PeerLibraryView{
				ID:             catResp.ID,
				Name:           name,
				BaseURL:        normBase,
				AllowDownloads: catResp.AllowDownloads,
				Items:          filtered,
			})
			mu.Unlock()
		}(baseURL)
	}
	wg.Wait()

	sort.Slice(peers, func(i, j int) bool {
		ni := strings.ToLower(peers[i].Name)
		nj := strings.ToLower(peers[j].Name)
		if ni != nj {
			return ni < nj
		}
		return peers[i].BaseURL < peers[j].BaseURL
	})
	if peers == nil {
		peers = []PeerLibraryView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peers": peers})
}

func (s *Server) handleAPIPeersPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.queue == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "job queue unavailable"})
		return
	}

	peerBase := strings.TrimRight(strings.TrimSpace(r.FormValue("peer_base_url")), "/")
	relRaw := strings.TrimSpace(r.FormValue("rel_path"))
	title := strings.TrimSpace(r.FormValue("title"))
	sizeBytes, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("size_bytes")), 10, 64)

	if (!strings.HasPrefix(peerBase, "http://") && !strings.HasPrefix(peerBase, "https://")) || relRaw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "peer_base_url and rel_path are required"})
		return
	}
	if !s.isKnownPeerBaseURL(peerBase) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden: peer_base_url is not a discovered or configured Ark peer"})
		return
	}
	subdir, cleanRel, ok := validatePeerRelPath(relRaw)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid rel_path"})
		return
	}

	fileName := filepath.Base(cleanRel)
	destPath := filepath.Join(s.dataDir, filepath.FromSlash(cleanRel))
	if st, err := os.Stat(destPath); err == nil && !st.IsDir() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"skipped": true,
			"msg":     "Already installed locally: " + fileName,
		})
		return
	}

	downloadURL := peerBase + "/api/peers/download?rel=" + url.QueryEscape(cleanRel)
	metaJSON, _ := json.Marshal(jobs.JobMetadata{
		OutputDir: filepath.Join(s.dataDir, filepath.FromSlash(subdir)),
		FileName:  fileName,
	})
	job := &jobs.Job{
		ID:         fmt.Sprintf("peer-%d", time.Now().UnixNano()),
		Type:       jobs.TypeDownload,
		Target:     downloadURL,
		Status:     jobs.StatusPending,
		TotalBytes: sizeBytes,
		Metadata:   string(metaJSON),
	}
	if err := s.queue.Submit(r.Context(), job); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	label := title
	if label == "" {
		label = fileName
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"job_id": job.ID,
		"msg":    "Queued download from peer: " + label,
	})
}

func (s *Server) isKnownPeerBaseURL(baseURL string) bool {
	norm := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if norm == "" {
		return false
	}
	found := false
	s.discoveredPeers.Range(func(_, v any) bool {
		if rec, ok := v.(discoveredPeerRecord); ok && strings.TrimRight(rec.BaseURL, "/") == norm {
			found = true
			return false
		}
		return true
	})
	if found {
		return true
	}
	s.peerByID.Range(func(_, v any) bool {
		if b, ok := v.(string); ok && strings.TrimRight(b, "/") == norm {
			found = true
			return false
		}
		return true
	})
	if found {
		return true
	}
	for _, m := range s.parseManualPeerHosts() {
		if strings.TrimRight(m, "/") == norm {
			return true
		}
	}
	return false
}

func isAllowedPeerProxyPath(targetPath string) bool {
	cleaned := path.Clean("/" + targetPath)
	for _, allowed := range []string{"/read/zim/", "/media/", "/api/library/icon", "/api/zim/", "/api/reader/", "/static/"} {
		if strings.HasPrefix(cleaned, allowed) {
			return true
		}
	}
	return false
}

func (s *Server) resolvePeerBaseByID(peerID string) (string, bool) {
	if v, ok := s.peerByID.Load(peerID); ok {
		if base, ok2 := v.(string); ok2 && base != "" {
			return base, true
		}
	}
	var found string
	s.discoveredPeers.Range(func(_, v any) bool {
		if rec, ok := v.(discoveredPeerRecord); ok && rec.InstanceID == peerID && rec.BaseURL != "" {
			found = rec.BaseURL
			return false
		}
		return true
	})
	if found != "" {
		s.peerByID.Store(peerID, found)
		return found, true
	}
	return "", false
}

func (s *Server) handlePeerProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/peer/")
	peerID, rest, hasSlash := strings.Cut(trimmed, "/")
	if peerID == "" {
		http.NotFound(w, r)
		return
	}
	if decoded, err := url.PathUnescape(peerID); err == nil && decoded != "" {
		peerID = decoded
	}
	targetPath := "/"
	if hasSlash {
		targetPath = "/" + rest
	}
	if !isAllowedPeerProxyPath(targetPath) {
		http.Error(w, "Forbidden: path not permitted through peer proxy.", http.StatusForbidden)
		return
	}
	baseURL, ok := s.resolvePeerBaseByID(peerID)
	if !ok {
		http.Error(w, "Peer Ark instance is not currently reachable.", http.StatusBadGateway)
		return
	}
	target, err := url.Parse(baseURL)
	if err != nil {
		http.Error(w, "Invalid peer URL", http.StatusBadGateway)
		return
	}
	prefix := "/peer/" + url.PathEscape(peerID)

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = lanPeerTransport()
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = targetPath
		req.URL.RawPath = ""
		req.URL.RawQuery = r.URL.RawQuery
		req.Host = target.Host
		req.Header.Del("Accept-Encoding")
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if loc := resp.Header.Get("Location"); loc != "" {
			if strings.HasPrefix(loc, baseURL+"/") {
				loc = strings.TrimPrefix(loc, baseURL)
			}
			if strings.HasPrefix(loc, "/read/zim/") || strings.HasPrefix(loc, "/media/") || strings.HasPrefix(loc, "/api/") {
				resp.Header.Set("Location", prefix+loc)
			}
		}
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		if strings.Contains(ct, "text/html") && resp.Body != nil {
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return err
			}
			for _, p := range []string{"/read/zim/", "/media/", "/api/zim/", "/api/library/", "/api/reader/"} {
				body = bytes.ReplaceAll(body, []byte(`"`+p), []byte(`"`+prefix+p))
				body = bytes.ReplaceAll(body, []byte(`'`+p), []byte(`'`+prefix+p))
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		}
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(rw, "Peer Ark instance unreachable.", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
