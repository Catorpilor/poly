package polymarket

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMaxTradePriceSince(t *testing.T) {
	t.Parallel()

	since := time.Unix(1_750_000_000, 0)
	point := func(t int64, p float64) string {
		return fmt.Sprintf(`{"t":%d,"p":%v}`, t, p)
	}

	tests := []struct {
		name      string
		status    int // 0 = 200 OK
		body      string
		wantPrice float64
		wantOK    bool
	}{
		{
			name: "normal series returns the max at or after since",
			body: `{"history":[` + strings.Join([]string{
				point(since.Unix()-600, 0.99), // pre-window high must not count
				point(since.Unix(), 0.42),     // t == since is in range
				point(since.Unix()+600, 0.495),
				point(since.Unix()+900, 0.18),
			}, ",") + `]}`,
			wantPrice: 0.495,
			wantOK:    true,
		},
		{
			name:   "empty history",
			body:   `{"history":[]}`,
			wantOK: false,
		},
		{
			name:   "malformed JSON",
			body:   `{"history":[{"t":not-a-number`,
			wantOK: false,
		},
		{
			name: "all points before since",
			body: `{"history":[` + strings.Join([]string{
				point(since.Unix()-600, 0.55),
				point(since.Unix()-60, 0.60),
			}, ",") + `]}`,
			wantOK: false,
		},
		{
			name:   "HTTP error status",
			status: http.StatusTooManyRequests,
			body:   `{"error":"rate limited"}`,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			tc := NewTradingClient(srv.URL, 137)
			price, ok := tc.MaxTradePriceSince(context.Background(), "tok-1", since)
			if ok != tt.wantOK || price != tt.wantPrice {
				t.Fatalf("MaxTradePriceSince = (%v, %v), want (%v, %v)", price, ok, tt.wantPrice, tt.wantOK)
			}
			if gotPath != "/prices-history" {
				t.Errorf("request path = %q, want /prices-history", gotPath)
			}
			for _, param := range []string{"market=tok-1", "interval=1d", "fidelity=5"} {
				if !strings.Contains(gotQuery, param) {
					t.Errorf("query %q missing %q", gotQuery, param)
				}
			}
		})
	}
}

func TestMaxTradePriceSinceUnreachableServer(t *testing.T) {
	t.Parallel()
	tc := NewTradingClient("http://127.0.0.1:1", 137)
	if price, ok := tc.MaxTradePriceSince(context.Background(), "tok-1", time.Unix(0, 0)); ok || price != 0 {
		t.Fatalf("MaxTradePriceSince on unreachable server = (%v, %v), want (0, false)", price, ok)
	}
}

// TestGetMarketByConditionIDRejectsUnfiltered locks in the identity check:
// Gamma silently ignores unknown query params and returns its default market
// list, so a wrong param name yields a plausible unrelated market instead of
// an error (production 2026-08-01: a LoL condition ID "resolved" to a
// politics market). The client must reject any response whose conditionId
// does not match the request.
func TestGetMarketByConditionIDRejectsUnfiltered(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate the filter being ignored: unrelated market, HTTP 200.
		fmt.Fprint(w, `[{"id":"9999","question":"Unrelated","conditionId":"0xdead","slug":"xi-jinping-out"}]`)
	}))
	defer srv.Close()

	mc := NewMarketClientWithURL(srv.URL)
	_, err := mc.GetMarketByConditionID(context.Background(), "0xf17e3c60c7ca0094aec6f7db5bcf058d8b8da68d7e01e9675fc2493e451237ac")
	if err == nil {
		t.Fatal("expected identity-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "filter not applied") {
		t.Errorf("error %q does not name the filter failure", err)
	}
}
