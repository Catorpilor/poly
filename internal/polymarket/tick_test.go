package polymarket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoundToTick(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		price float64
		tick  float64
		want  float64
	}{
		{"1c tick keeps 0.05", 0.05, 0.01, 0.05},
		{"1c tick rounds 0.051 down to 0.05", 0.051, 0.01, 0.05},
		{"0.1c tick keeps 0.051", 0.051, 0.001, 0.051},
		{"0.1c tick rounds 0.0515 up to 0.052", 0.0515, 0.001, 0.052},
		{"0.1c tick rounds 0.0514 down to 0.051", 0.0514, 0.001, 0.051},
		{"1c tick keeps 0.16", 0.16, 0.01, 0.16},
		{"zero tick is no-op", 0.05, 0, 0.05},
		{"negative tick is no-op", 0.05, -0.01, 0.05},
		{"float fuzz handled cleanly (0.1+0.2 with 0.01 tick)", 0.1 + 0.2, 0.01, 0.30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := roundToTick(tt.price, tt.tick)
			// Compare within a tight tolerance — floats from arithmetic, not strings.
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("roundToTick(%v, %v) = %v, want %v", tt.price, tt.tick, got, tt.want)
			}
		})
	}
}

// TestGetMinimumTickSize covers the lookup helper. It mirrors GetMarketInfo's
// two-call shape (/book?token_id=… → /markets/<conditionID>) and asserts the
// per-market tick is returned, with a sensible default when the API omits it.
func TestGetMinimumTickSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		marketJSON  map[string]any
		want        float64
		wantWarning bool
	}{
		{
			name:       "low-priced market returns 0.001",
			marketJSON: map[string]any{"minimum_tick_size": 0.001},
			want:       0.001,
		},
		{
			name:       "standard market returns 0.01",
			marketJSON: map[string]any{"minimum_tick_size": 0.01},
			want:       0.01,
		},
		{
			name:        "missing tick falls back to 0.01",
			marketJSON:  map[string]any{},
			want:        0.01,
			wantWarning: true,
		},
		{
			name:        "zero tick falls back to 0.01",
			marketJSON:  map[string]any{"minimum_tick_size": 0.0},
			want:        0.01,
			wantWarning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conditionID := "0xabc"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/book":
					json.NewEncoder(w).Encode(map[string]any{"market": conditionID})
				case "/markets/" + conditionID:
					json.NewEncoder(w).Encode(tt.marketJSON)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			tc := NewTradingClient(server.URL, 137)
			got, err := tc.GetMinimumTickSize(context.Background(), "tok-1")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("GetMinimumTickSize = %v, want %v", got, tt.want)
			}
		})
	}
}
