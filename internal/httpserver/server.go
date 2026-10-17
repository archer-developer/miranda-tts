// Package httpserver wires the service's HTTP handler behind a single mux.
// It is transport-agnostic about what mcpHandler and audioHandler actually
// are — callers pass in whatever handlers serve MCP over Streamable HTTP and
// the rendered audio clips.
package httpserver

import (
	"net/http"
)

// New builds the top-level HTTP handler:
//   - GET /healthz         — unauthenticated liveness check.
//   - /mcp                 — the MCP endpoint, bearer-auth-gated.
//   - GET /audio/{filename} — rendered speech clips, deliberately
//     UNAUTHENTICATED: the media players that fetch them cannot present a
//     bearer token. The filename is a content hash validated by audioHandler
//     before it ever reaches the filesystem. This third route is the one
//     deliberate deviation from the skeleton's two-route mux.
//
// token is the shared secret that MCP clients must present.
func New(mcpHandler http.Handler, token string, audioHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/mcp", requireBearerToken(token, mcpHandler))
	mux.Handle("GET /audio/{filename}", audioHandler)
	return mux
}
