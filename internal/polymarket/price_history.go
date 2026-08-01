package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// priceHistoryResponse mirrors the CLOB's public GET /prices-history payload:
// a series of (unix timestamp, traded price) points.
type priceHistoryResponse struct {
	History []struct {
		T int64   `json:"t"`
		P float64 `json:"p"`
	} `json:"history"`
}

// MaxTradePriceSince returns the highest traded price for tokenID at or after
// since, from the CLOB's public price history (no L2 auth). Used to seed the
// snipe watcher's Session High for tokens that join the watch mid-game.
// Returns (0, false) when the request fails, the response is malformed, or no
// point falls at or after since — callers treat that as "no seed", never as
// a real price.
func (tc *TradingClient) MaxTradePriceSince(ctx context.Context, tokenID string, since time.Time) (float64, bool) {
	u := fmt.Sprintf("%s/prices-history?market=%s&interval=1d&fidelity=5",
		tc.clobURL, url.QueryEscape(tokenID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false
	}
	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}

	var parsed priceHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, false
	}

	cutoff := since.Unix()
	var max float64
	found := false
	for _, pt := range parsed.History {
		if pt.T >= cutoff && (!found || pt.P > max) {
			max = pt.P
			found = true
		}
	}
	return max, found
}
