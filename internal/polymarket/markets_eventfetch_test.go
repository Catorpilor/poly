package polymarket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GetEventMarkets (issue #94): fetches an event's embedded markets and stamps
// the requested slug so callers can group series-mates.
func TestGetEventMarkets(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" || r.URL.Query().Get("slug") != "cs2-a-b-2026" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[{"slug":"cs2-a-b-2026","markets":[
			{"id":"m1","question":"A vs B - Map 1 Winner","outcomes":"[\"A\",\"B\"]","clobTokenIds":"[\"t1a\",\"t1b\"]","gameStartTime":"2026-08-23 12:15:00+00","active":true,"closed":false},
			{"id":"m2","question":"Total Kills Over/Under 50.5 in Game 1?","outcomes":"[\"Over\",\"Under\"]","clobTokenIds":"[\"t2a\",\"t2b\"]","active":true,"closed":false}
		]}]`)
	}))
	t.Cleanup(srv.Close)

	mc := NewMarketClientWithURL(srv.URL)
	markets, err := mc.GetEventMarkets(context.Background(), "cs2-a-b-2026")
	if err != nil {
		t.Fatalf("GetEventMarkets: %v", err)
	}
	if len(markets) != 2 {
		t.Fatalf("markets = %d, want 2", len(markets))
	}
	m := markets[0]
	if m.ID != "m1" || m.Question != "A vs B - Map 1 Winner" || !m.Active || m.Closed {
		t.Errorf("market[0] = %+v, want m1 fields decoded", m)
	}
	if got := m.GetClobTokenIds(); len(got) != 2 || got[0] != "t1a" {
		t.Errorf("tokens = %v, want [t1a t1b]", got)
	}
	if m.GetGameStartTime().IsZero() {
		t.Errorf("gameStartTime not decoded — inert watches would never alert")
	}
	if m.GetEventSlug() != "cs2-a-b-2026" {
		t.Errorf("event slug = %q, want the requested slug stamped", m.GetEventSlug())
	}
}

func TestGetEventMarketsNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	if _, err := NewMarketClientWithURL(srv.URL).GetEventMarkets(context.Background(), "nope"); err == nil {
		t.Fatal("want an error for an unknown event slug")
	}
}
