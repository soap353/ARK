package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// LANAllowlistEntry represents an IP address approved via LAN passcode or Settings.
type LANAllowlistEntry struct {
	IP        string `json:"ip"`
	Label     string `json:"label"`
	CreatedAt string `json:"created_at"`
}

type rateBucket struct {
	count   int
	resetAt time.Time
}

type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]rateBucket
}

func newIPRateLimiter() *ipRateLimiter {
	return &ipRateLimiter{buckets: make(map[string]rateBucket)}
}

func (l *ipRateLimiter) allow(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok || now.After(b.resetAt) {
		l.buckets[key] = rateBucket{count: 1, resetAt: now.Add(window)}
		return true
	}
	if b.count >= limit {
		return false
	}
	b.count++
	l.buckets[key] = b
	return true
}

func (l *ipRateLimiter) recordFailure(key string, maxFailures int, window time.Duration) bool {
	return !l.allow(key, maxFailures, window)
}

func (l *ipRateLimiter) isBlocked(key string, maxFailures int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok || time.Now().After(b.resetAt) {
		return false
	}
	return b.count >= maxFailures
}

func (l *ipRateLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.buckets, key)
	l.mu.Unlock()
}

func (s *Server) ensureSecurityTables() {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS lan_allowlist (
			ip TEXT PRIMARY KEY,
			label TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
}

func hashPasscode(pin, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + pin))
	return hex.EncodeToString(sum[:])
}

func newSalt() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func (s *Server) setLANPasscode(pin string) error {
	salt := newSalt()
	h := hashPasscode(pin, salt)
	return s.saveSettings(map[string]string{
		"lan_passcode_salt": salt,
		"lan_passcode_hash": h,
	})
}

func (s *Server) verifyLANPasscode(pin string) bool {
	cfg := s.fetchSettings()
	salt := cfg["lan_passcode_salt"]
	want := cfg["lan_passcode_hash"]
	if want == "" {
		return false
	}
	got := hashPasscode(pin, salt)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) isLANPasscodeRequired() bool {
	if !s.isSettingEnabled("lan_passcode_enabled", false) {
		return false
	}
	cfg := s.fetchSettings()
	return cfg["lan_passcode_hash"] != ""
}

func normalizeClientIP(raw string) string {
	host := strings.TrimSpace(raw)
	if host == "" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4.String()
		}
		return ip.String()
	}
	return host
}

func (s *Server) isIPAllowlisted(ip string) bool {
	ip = normalizeClientIP(ip)
	if s.db == nil || ip == "" {
		return false
	}
	s.ensureSecurityTables()
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM lan_allowlist WHERE ip = ? LIMIT 1`, ip).Scan(&exists)
	return err == nil && exists == 1
}

func (s *Server) addIPToAllowlist(ip, label string) {
	ip = normalizeClientIP(ip)
	if s.db == nil || ip == "" {
		return
	}
	s.ensureSecurityTables()
	_, _ = s.db.Exec(`
		INSERT INTO lan_allowlist (ip, label, created_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(ip) DO UPDATE SET label = CASE WHEN excluded.label != '' THEN excluded.label ELSE lan_allowlist.label END
	`, ip, label)
}

func (s *Server) removeIPFromAllowlist(ip string) {
	ip = normalizeClientIP(ip)
	if s.db == nil || ip == "" {
		return
	}
	s.ensureSecurityTables()
	_, _ = s.db.Exec(`DELETE FROM lan_allowlist WHERE ip = ?`, ip)
}

func (s *Server) listAllowlistEntries() []LANAllowlistEntry {
	if s.db == nil {
		return nil
	}
	s.ensureSecurityTables()
	rows, err := s.db.Query(`SELECT ip, label, created_at FROM lan_allowlist ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []LANAllowlistEntry
	for rows.Next() {
		var e LANAllowlistEntry
		if err := rows.Scan(&e.IP, &e.Label, &e.CreatedAt); err == nil {
			out = append(out, e)
		}
	}
	return out
}

func isLoopbackOriginOrHost(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// WrapWithSecurity adds HTTP security headers, local CSRF origin checks, LAN toggle enforcement,
// optional LAN passcode allowlist gating, and remote rate-limiting.
func (s *Server) WrapWithSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Standard browser security headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")

		isLocal := isLocalRequest(r)

		// 2. Local CSRF protection: reject non-loopback or opaque ("null") Origin,
		// and when Origin is absent, reject non-loopback Referer on state-changing local requests.
		if isLocal && (r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete) {
			if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
				if origin == "null" || !isLoopbackOriginOrHost(origin) {
					http.Error(w, "Forbidden: cross-origin state change rejected.", http.StatusForbidden)
					return
				}
			} else if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
				if !isLoopbackOriginOrHost(ref) {
					http.Error(w, "Forbidden: cross-origin state change rejected.", http.StatusForbidden)
					return
				}
			}
		}

		// 3. Remote LAN checks
		if !isLocal {
			if !s.isSettingEnabled("lan_enabled", true) {
				http.Error(w, "Forbidden: LAN browsing is disabled in Settings.", http.StatusForbidden)
				return
			}

			p := r.URL.Path
			// Allow static assets, healthz, and peer discovery endpoints (governed by their own Settings toggles)
			isExempt := strings.HasPrefix(p, "/static/") || p == "/healthz" || p == "/lan-auth" ||
				p == "/api/peers/catalog" || p == "/api/peers/download"

			if !isExempt && s.isLANPasscodeRequired() {
				ip := clientHostFromReq(r)
				if !s.isIPAllowlisted(ip) {
					if strings.HasPrefix(p, "/api/") {
						writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "LAN passcode required"})
						return
					}
					http.Redirect(w, r, "/lan-auth?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
					return
				}
			}

			// Remote rate-limiting for write/AI endpoints
			if r.Method == http.MethodPost {
				ip := clientHostFromReq(r)
				if p == "/api/lan-drop/post" && !s.lanRateLimiter.allow("ld:"+ip, 20, time.Minute) {
					writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "Rate limit exceeded. Try again in a minute."})
					return
				}
				if p == "/api/ai/chat" && !s.lanRateLimiter.allow("ai:"+ip, 20, time.Minute) {
					writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "Rate limit exceeded. Try again in a minute."})
					return
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLANAuth(w http.ResponseWriter, r *http.Request) {
	if isLocalRequest(r) || !s.isLANPasscodeRequired() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	nextURL := sanitizeLocalRedirect(r.URL.Query().Get("next"))
	if nextURL == "" {
		nextURL = "/"
	}
	ip := clientHostFromReq(r)
	errMsg := ""

	if r.Method == http.MethodPost {
		if f := sanitizeLocalRedirect(r.FormValue("next")); f != "" {
			nextURL = f
		}
		if s.pinFailLimiter.isBlocked("pin:"+ip, 5) {
			w.WriteHeader(http.StatusTooManyRequests)
			errMsg = "Too many failed attempts. Please wait 5 minutes and try again."
		} else {
			pin := strings.TrimSpace(r.FormValue("passcode"))
			if s.verifyLANPasscode(pin) {
				s.pinFailLimiter.reset("pin:" + ip)
				s.addIPToAllowlist(ip, detectDeviceKind(r.UserAgent()))
				http.Redirect(w, r, nextURL, http.StatusSeeOther)
				return
			}
			s.pinFailLimiter.recordFailure("pin:"+ip, 5, 5*time.Minute)
			w.WriteHeader(http.StatusUnauthorized)
			errMsg = "Incorrect passcode."
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	errHTML := ""
	if errMsg != "" {
		errHTML = `<div style="color:#B91C1C;background:#FEE2E2;padding:0.65rem 0.85rem;border-radius:6px;margin-bottom:0.9rem;font-size:14px;">` + html.EscapeString(errMsg) + `</div>`
	}
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Ark LAN Access</title>
  <link rel="stylesheet" href="/static/ark.css">
</head>
<body style="display:flex;align-items:center;justify-content:center;min-height:100vh;padding:1.5rem;">
  <div class="card" style="max-width:420px;width:100%%;">
    <h1 style="margin:0 0 0.35rem;font-size:22px;">Ark LAN Access</h1>
    <p class="secondary-text" style="margin:0 0 1rem;font-size:14px;">Enter the host passcode to unlock access for your device (<code>%s</code>).</p>
    %s
    <form action="/lan-auth" method="POST" style="display:flex;flex-direction:column;gap:0.85rem;">
      <input type="hidden" name="next" value="%s">
      <input type="password" name="passcode" class="form-control" placeholder="Enter LAN Passcode" autofocus required>
      <button type="submit" class="btn btn-primary">Unlock Device</button>
    </form>
  </div>
</body>
</html>`, html.EscapeString(ip), errHTML, html.EscapeString(nextURL))
}

func (s *Server) handleSettingsAllowlistRemove(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	ip := strings.TrimSpace(r.FormValue("ip"))
	if ip != "" {
		s.removeIPFromAllowlist(ip)
	}
	http.Redirect(w, r, "/settings?msg="+url.QueryEscape("Removed "+ip+" from trusted LAN allowlist."), http.StatusSeeOther)
}

func (s *Server) handleSettingsAllowlistAdd(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	rawIP := strings.TrimSpace(r.FormValue("ip"))
	parsed := net.ParseIP(rawIP)
	if parsed == nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Invalid IP address: "+rawIP), http.StatusSeeOther)
		return
	}
	ip := normalizeClientIP(rawIP)
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		label = "Manual"
	}
	s.addIPToAllowlist(ip, label)
	http.Redirect(w, r, "/settings?msg="+url.QueryEscape("Added "+ip+" to trusted LAN allowlist."), http.StatusSeeOther)
}
