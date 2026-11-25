package apps

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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

		// Request uncompressed HTML so the proxy can cleanly inject the shared Ark navigation bar.
		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header.Set("User-Agent", "Ark-Proxy")
		}
		req.Header.Del("Accept-Encoding")
	}

	errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
		writeAppStatusPage(w, http.StatusBadGateway,
			"App Unreachable — Ark",
			"App Module Unavailable",
			fmt.Sprintf(`<p>The module <strong>%s</strong> is still starting up or temporarily unreachable.</p>
  <p><a href="javascript:location.reload()">Click here to retry</a> or return to <a href="/">Ark Home</a>.</p>`, html.EscapeString(appID)))
	}

	proxy := &httputil.ReverseProxy{
		Director:       director,
		FlushInterval:  100 * time.Millisecond,
		ErrorHandler:   errorHandler,
		ModifyResponse: injectArkAppNavbar(appID),
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
		http.Redirect(w, r, "/a/"+url.PathEscape(appID)+"/", http.StatusPermanentRedirect)
		return
	}

	pm.mu.RLock()
	route, ok := pm.routes[appID]
	pm.mu.RUnlock()

	if !ok || route == nil || route.Proxy == nil {
		writeAppStatusPage(w, http.StatusNotFound,
			"App Not Running — Ark",
			"App Not Active",
			fmt.Sprintf(`<p>The module <strong>%s</strong> is not currently running.</p>
  <p><a href="/apps">Manage Apps in Settings</a> or return to <a href="/">Ark Home</a>.</p>`, html.EscapeString(appID)))
		return
	}

	route.Proxy.ServeHTTP(w, r)
}

func injectArkAppNavbar(appID string) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK || resp.ContentLength > 4*1024*1024 {
			return nil
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(strings.ToLower(ct), "text/html") || resp.Header.Get("Content-Encoding") != "" {
			return nil
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		resp.Body.Close()
		if err != nil {
			return err
		}

		extraBtn := ""
		if appID == "kiwix" {
			extraBtn = `<button type="button" onclick="var b=this;b.disabled=true;b.textContent='Adding...';var c=document.body.cloneNode(true);var n=c.querySelector('#ark-proxy-nav');if(n)n.remove();var txt=(c.innerText||'').trim().slice(0,800000);fetch('/api/ai/index-article',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({url:location.pathname,title:document.title,text:txt})}).then(async r=>{if(r.ok){var d=await r.json().catch(()=>({}));b.textContent='Added to AI'+(d.indexed_chunks?' ('+d.indexed_chunks+')':'');}else{var t=await r.text();b.textContent='Error';b.title=t;b.disabled=false;}}).catch(e=>{b.textContent='Error';b.title=e.message;b.disabled=false;});" style="margin-left:auto;background:#C25E28;color:#FDFBF7;border:none;border-radius:6px;padding:4px 12px;font-size:13px;font-weight:600;cursor:pointer;white-space:nowrap;flex-shrink:0;">+ Add to AI</button>`
		}

		navbar := `<nav id="ark-proxy-nav" style="position:sticky;top:0;z-index:99999;background:#1B2E26;border-bottom:1px solid #2A4338;padding:8px 16px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;display:flex;align-items:center;gap:14px;overflow-x:auto;box-shadow:0 2px 6px rgba(27,46,38,.18);line-height:1.3;">` +
			`<a href="/" style="color:#F5F2EB;font-weight:700;text-decoration:none;font-size:15px;white-space:nowrap;flex-shrink:0;">&#8592; Ark Home</a>` +
			`<span style="color:#436153;flex-shrink:0;">|</span>` +
			`<a href="/library" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:14px;white-space:nowrap;flex-shrink:0;">Library</a>` +
			`<a href="/kiwix" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:14px;white-space:nowrap;flex-shrink:0;">Kiwix Catalog</a>` +
			`<a href="/get-content" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:14px;white-space:nowrap;flex-shrink:0;">Get Content</a>` +
			`<a href="/field-guide" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:14px;white-space:nowrap;flex-shrink:0;">Field Guide</a>` +
			`<a href="/apps" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:14px;white-space:nowrap;flex-shrink:0;">Apps</a>` +
			`<a href="/maps" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:14px;white-space:nowrap;flex-shrink:0;">Maps</a>` +
			`<a href="/ask" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:14px;white-space:nowrap;flex-shrink:0;">Ask AI</a>` +
			`<a href="/system" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:14px;white-space:nowrap;flex-shrink:0;">System</a>` +
			`<a href="/settings" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:14px;white-space:nowrap;flex-shrink:0;">Settings</a>` +
			extraBtn + `</nav>`

		replaced := insertAfterBodyOpen(body, []byte(navbar))
		resp.Body = io.NopCloser(bytes.NewReader(replaced))
		resp.ContentLength = int64(len(replaced))
		resp.Header.Set("Content-Length", strconv.Itoa(len(replaced)))
		return nil
	}
}

func insertAfterBodyOpen(htmlBytes, snippet []byte) []byte {
	lower := bytes.ToLower(htmlBytes)
	idx := bytes.Index(lower, []byte("<body"))
	if idx >= 0 {
		end := bytes.IndexByte(lower[idx:], '>')
		if end >= 0 {
			pos := idx + end + 1
			out := make([]byte, 0, len(htmlBytes)+len(snippet))
			out = append(out, htmlBytes[:pos]...)
			out = append(out, snippet...)
			out = append(out, htmlBytes[pos:]...)
			return out
		}
	}
	return bytes.Replace(htmlBytes, []byte("</body>"), append(snippet, []byte("</body>")...), 1)
}

func writeAppStatusPage(w http.ResponseWriter, status int, title, heading, bodyHTML string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>%s</title>
<style>
body { background: #F4F6F9; color: #0F172A; font-family: system-ui, sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; }
.box { text-align: center; max-width: 480px; padding: 32px; background: #FFFFFF; border-radius: 8px; border: 1px solid #CBD5E1; box-shadow: 0 2px 6px rgba(15, 23, 42, 0.06); }
h1 { color: #0369A1; font-size: 1.5rem; margin-bottom: 12px; }
p { color: #475569; line-height: 1.5; }
a { color: #0369A1; text-decoration: none; font-weight: 600; }
</style>
</head>
<body>
<div class="box">
  <h1>%s</h1>
  %s
</div>
</body>
</html>`, title, heading, bodyHTML)
}
