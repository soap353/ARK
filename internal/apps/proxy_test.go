package apps

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProxyManager_Routing(t *testing.T) {
	// 1. Create a dummy backend server
	backendReceivedPath := ""
	backendReceivedHeader := ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendReceivedPath = r.URL.Path
		backendReceivedHeader = r.Header.Get("X-Forwarded-Prefix")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backend ok"))
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("failed to parse backend url: %v", err)
	}

	pm := NewProxyManager()

	// Register with stripPrefix = true
	pm.Register("cyberchef", backendURL, true)

	// Register another with stripPrefix = false (e.g. kiwix)
	pm.Register("kiwix", backendURL, false)

	// Test 1: cyberchef with stripPrefix = true
	req1 := httptest.NewRequest("GET", "/a/cyberchef/index.html", nil)
	rec1 := httptest.NewRecorder()
	pm.ServeHTTP(rec1, req1)

	resp1 := rec1.Result()
	body1, _ := io.ReadAll(resp1.Body)
	if resp1.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp1.StatusCode)
	}
	if string(body1) != "backend ok" {
		t.Errorf("expected 'backend ok', got %q", string(body1))
	}
	if backendReceivedPath != "/index.html" {
		t.Errorf("expected backend to receive '/index.html', got %q", backendReceivedPath)
	}
	if backendReceivedHeader != "/a/cyberchef" {
		t.Errorf("expected prefix header '/a/cyberchef', got %q", backendReceivedHeader)
	}

	// Test 2: kiwix with stripPrefix = false
	req2 := httptest.NewRequest("GET", "/a/kiwix/wikipedia_en/A", nil)
	rec2 := httptest.NewRecorder()
	pm.ServeHTTP(rec2, req2)

	resp2 := rec2.Result()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp2.StatusCode)
	}
	if backendReceivedPath != "/a/kiwix/wikipedia_en/A" {
		t.Errorf("expected backend to receive full path '/a/kiwix/wikipedia_en/A', got %q", backendReceivedPath)
	}

	// Test 3: Unregistered app returns 404
	req3 := httptest.NewRequest("GET", "/a/unknown/foo", nil)
	rec3 := httptest.NewRecorder()
	pm.ServeHTTP(rec3, req3)

	resp3 := rec3.Result()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for unregistered app, got %d", resp3.StatusCode)
	}

	// Test 4: Trailing slash redirect
	req4 := httptest.NewRequest("GET", "/a/cyberchef", nil)
	rec4 := httptest.NewRecorder()
	pm.ServeHTTP(rec4, req4)

	resp4 := rec4.Result()
	if resp4.StatusCode != http.StatusPermanentRedirect {
		t.Errorf("expected 308 redirect for /a/cyberchef, got %d", resp4.StatusCode)
	}
	if loc := resp4.Header.Get("Location"); loc != "/a/cyberchef/" {
		t.Errorf("expected redirect location '/a/cyberchef/', got %q", loc)
	}
}
