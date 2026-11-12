package apps

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AppRoute holds the reverse proxy and target configuration for a running app module.
type AppRoute struct {
	AppID       string
	Target      *url.URL
	StripPrefix bool
	Proxy       *httputil.ReverseProxy
}

// ProxyManager manages reverse proxies for all running app modules mounted under /a/<id>/.
type ProxyManager struct {
	mu     sync.RWMutex
	routes map[string]*AppRoute
}

// NewProxyManager creates a new ProxyManager.
func NewProxyManager() *ProxyManager {
	return &ProxyManager{
		routes: make(map[string]*AppRoute),
	}
}

// Register registers or updates a reverse proxy target for an app ID.
// If stripPrefix is true, /a/<id> is stripped from the URL path before proxying.
// For apps like kiwix configured with --urlRootLocation /a/<id>, stripPrefix should be false.
func (pm *ProxyManager) Register(appID string, targetURL *url.URL, stripPrefix bool) *httputil.ReverseProxy {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	director := func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host

		if stripPrefix {
			prefix := "/a/" + appID
			if strings.HasPrefix(req.URL.Path, prefix) {
				req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
				if !strings.HasPrefix(req.URL.Path, "/") {
					req.URL.Path = "/" + req.URL.Path
				}
			}
		}


		// Forwarding headers
		if req.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Header.Set("X-Forwarded-Prefix", "/a/"+appID)

		// Ensure connection headers are maintained for websockets
		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header.Set("User-Agent", "Ark-Proxy")
		}
	}

	errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>App Unreachable — Ark</title>
<style>
body { background: #0B0D10; color: #F4F4F0; font-family: sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; }
.box { text-align: center; max-width: 480px; padding: 32px; background: #15181D; border-radius: 8px; border: 1px solid #2A2F37; }
h1 { color: #FFB000; font-size: 1.5rem; margin-bottom: 12px; }
p { color: #80868B; line-height: 1.5; }
a { color: #FFB000; text-decoration: none; }
</style>
</head>
<body>
<div class="box">
  <h1>App Module Unavailable</h1>
  <p>The module <strong>%s</strong> is still starting up or temporarily unreachable.</p>
  <p><a href="javascript:location.reload()">Click here to retry</a> or return to <a href="/">Ark Home</a>.</p>
</div>
</body>
</html>`, appID)
	}

	proxy := &httputil.ReverseProxy{
		Director:      director,
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler:  errorHandler,
	}

	pm.routes[appID] = &AppRoute{
		AppID:       appID,
		Target:      targetURL,
		StripPrefix: stripPrefix,
		Proxy:       proxy,
	}

	return proxy
}

// Unregister removes the reverse proxy route for an app.
func (pm *ProxyManager) Unregister(appID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.routes, appID)
}

// GetRoute returns the AppRoute for an app ID, if registered.
func (pm *ProxyManager) GetRoute(appID string) (*AppRoute, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	route, ok := pm.routes[appID]
	return route, ok
}

// ServeHTTP inspects requests matching /a/<id>/ and delegates to the registered reverse proxy.
func (pm *ProxyManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/a/") {
		http.NotFound(w, r)
		return
	}

	// Extract app ID from /a/<id> or /a/<id>/...
	subPath := strings.TrimPrefix(path, "/a/")
	parts := strings.SplitN(subPath, "/", 2)
	appID := parts[0]

	if appID == "" {
		http.NotFound(w, r)
		return
	}

	// If request is strictly /a/<id> without trailing slash, redirect to /a/<id>/
	if len(parts) == 1 && !strings.HasSuffix(path, "/") {
		http.Redirect(w, r, "/a/"+appID+"/", http.StatusPermanentRedirect)
		return
	}

	pm.mu.RLock()
	route, ok := pm.routes[appID]
	pm.mu.RUnlock()

	if !ok || route == nil || route.Proxy == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>App Not Running — Ark</title>
<style>
body { background: #0B0D10; color: #F4F4F0; font-family: sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; }
.box { text-align: center; max-width: 480px; padding: 32px; background: #15181D; border-radius: 8px; border: 1px solid #2A2F37; }
h1 { color: #FFB000; font-size: 1.5rem; margin-bottom: 12px; }
p { color: #80868B; line-height: 1.5; }
a { color: #FFB000; text-decoration: none; }
</style>
</head>
<body>
<div class="box">
  <h1>App Not Active</h1>
  <p>The module <strong>%s</strong> is not currently running.</p>
  <p><a href="/apps">Manage Apps in Settings</a> or return to <a href="/">Ark Home</a>.</p>
</div>
</body>
</html>`, appID)
		return
	}

	route.Proxy.ServeHTTP(w, r)
}
