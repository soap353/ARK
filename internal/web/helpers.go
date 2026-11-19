package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// requireMethod ensures the request uses the expected HTTP method, writing 405 otherwise.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// writeJSON serializes v as JSON with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError writes a JSON object containing an "error" message with the given status code.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// redirectWithQuery redirects the client with 303 See Other and an escaped query parameter.
func redirectWithQuery(w http.ResponseWriter, r *http.Request, basePath, key, value string) {
	sep := "?"
	if strings.Contains(basePath, "?") {
		sep = "&"
	}
	http.Redirect(w, r, basePath+sep+key+"="+url.QueryEscape(value), http.StatusSeeOther)
}
