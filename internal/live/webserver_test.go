package live

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/config"
)

// The web server is LAN-only by design (no bearer auth — see tasks/todo.md
// F1/Q1). The network boundary doesn't stop a browser on the LAN from being
// used as a CSRF proxy, so every /api/* request must satisfy:
//   - Host is localhost, an IP literal, or the LIVE_WEB_URL host
//     (blocks DNS rebinding, where Host is the attacker's domain), and
//   - any Origin header matches the request Host exactly
//     (blocks cross-site fetches from pages this server didn't serve), and
//   - POST bodies declare Content-Type: application/json
//     (forces a CORS preflight on cross-origin fetches, which then fails).
func TestAPIRequestGuard(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.App.LiveWebURL = "http://pi.local:8081"
	ws := NewWebServer(nil, 0, nil, cfg, nil, nil)
	handler := ws.httpServer.Handler

	tests := []struct {
		name        string
		method      string
		path        string
		host        string
		origin      string
		contentType string
		wantStatus  int
	}{
		// Legitimate access. 503 = the guard passed and handleTrade hit its
		// "trading not configured" check (nil deps in this test server).
		{
			name:        "localhost accepted",
			method:      "POST",
			path:        "/api/trade",
			host:        "localhost:8081",
			contentType: "application/json",
			wantStatus:  http.StatusServiceUnavailable,
		},
		{
			name:        "LAN IP with matching origin accepted",
			method:      "POST",
			path:        "/api/trade",
			host:        "192.168.1.5:8081",
			origin:      "http://192.168.1.5:8081",
			contentType: "application/json",
			wantStatus:  http.StatusServiceUnavailable,
		},
		{
			name:        "configured LIVE_WEB_URL host accepted",
			method:      "POST",
			path:        "/api/trade",
			host:        "pi.local:8081",
			contentType: "application/json",
			wantStatus:  http.StatusServiceUnavailable,
		},
		{
			name:        "charset parameter on content type accepted",
			method:      "POST",
			path:        "/api/trade",
			host:        "localhost:8081",
			contentType: "application/json; charset=utf-8",
			wantStatus:  http.StatusServiceUnavailable,
		},

		// Cross-site fetch: Origin names a site that isn't this server.
		{
			name:        "cross-site origin rejected",
			method:      "POST",
			path:        "/api/trade",
			host:        "192.168.1.5:8081",
			origin:      "https://evil.example",
			contentType: "application/json",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "origin on a different port rejected",
			method:      "POST",
			path:        "/api/trade",
			host:        "192.168.1.5:8081",
			origin:      "http://192.168.1.5:3000",
			contentType: "application/json",
			wantStatus:  http.StatusForbidden,
		},

		// DNS rebinding: attacker.com resolves to this server's IP, so the
		// request is same-origin from the browser's view — Host gives it away.
		{
			name:        "dns-rebound hostname rejected",
			method:      "POST",
			path:        "/api/trade",
			host:        "attacker.example:8081",
			origin:      "http://attacker.example:8081",
			contentType: "application/json",
			wantStatus:  http.StatusForbidden,
		},

		// Preflight-dodging content types.
		{
			name:        "text/plain body rejected",
			method:      "POST",
			path:        "/api/trade",
			host:        "localhost:8081",
			contentType: "text/plain",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "missing content type rejected",
			method:      "POST",
			path:        "/api/trade",
			host:        "localhost:8081",
			contentType: "",
			wantStatus:  http.StatusUnsupportedMediaType,
		},

		// The guard covers every /api/ endpoint, not just /api/trade.
		{
			name:        "auth init guarded",
			method:      "POST",
			path:        "/api/auth/init",
			host:        "attacker.example:8081",
			contentType: "application/json",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "auth complete guarded",
			method:      "POST",
			path:        "/api/auth/complete",
			host:        "192.168.1.5:8081",
			origin:      "https://evil.example",
			contentType: "application/json",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:       "auth status GET guarded against bad origin",
			method:     "GET",
			path:       "/api/auth/status",
			host:       "192.168.1.5:8081",
			origin:     "https://evil.example",
			wantStatus: http.StatusForbidden,
		},
		// GET has no body, so the JSON content-type rule must not apply.
		// 400 = guard passed, handler rejected the missing token param.
		{
			name:       "auth status GET needs no content type",
			method:     "GET",
			path:       "/api/auth/status",
			host:       "localhost:8081",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var body *strings.Reader
			if tt.method == http.MethodPost {
				body = strings.NewReader("{}")
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tt.method, "http://"+tt.host+tt.path, body)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("%s %s (Host=%s Origin=%q CT=%q) = %d, want %d; body: %s",
					tt.method, tt.path, tt.host, tt.origin, tt.contentType,
					rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// The WebSocket upgrade must apply the same origin predicate: a page this
// server didn't serve must not be able to open the live feed socket.
func TestWebSocketUpgradeRejectsForeignOrigin(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.App.LiveWebURL = "http://localhost:8081"
	ws := NewWebServer(nil, 0, nil, cfg, nil, nil)

	req := httptest.NewRequest("GET", "http://192.168.1.5:8081/ws", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	rec := httptest.NewRecorder()
	ws.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("WS upgrade with foreign origin = %d, want %d; body: %s",
			rec.Code, http.StatusForbidden, rec.Body.String())
	}
}
