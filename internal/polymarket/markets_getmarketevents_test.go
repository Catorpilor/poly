package polymarket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GetMarketEvents (issue #99): the path form GET /markets/{id} omits events[],
// so the series walk keys off a slug that is never present. GetMarketEvents does
// ONE list-form fetch GET /markets?id={id} and returns the first element's
// events — the slice callers graft onto the path-form market to revive the walk.
func TestGetMarketEventsListFormCarriesEvents(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path + "?" + r.URL.RawQuery
		if r.URL.Path == "/markets" && r.URL.Query().Get("id") == "3682196" {
			// The list form carries events[] for in-play sports markets.
			fmt.Fprint(w, `[{"id":"3682196","question":"A vs B - Map 1 Winner","events":[{"id":"e1","slug":"cs2-a-b-2026","title":"A vs B"}]}]`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	events, err := NewMarketClientWithURL(srv.URL).GetMarketEvents(context.Background(), "3682196")
	if err != nil {
		t.Fatalf("GetMarketEvents: %v", err)
	}
	if got != "/markets?id=3682196" {
		t.Errorf("request = %q, want the list form /markets?id=3682196", got)
	}
	if len(events) != 1 || events[0].Slug != "cs2-a-b-2026" {
		t.Fatalf("events = %+v, want one event with slug cs2-a-b-2026", events)
	}
}

// The list form is UNRELIABLE at lifecycle edges (empty for brand-new and
// recently-closed markets). An empty array is NOT an error — it must fail open:
// nil events, nil error, so the caller keeps the un-enriched path-form market.
func TestGetMarketEventsEmptyListIsNotAnError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	events, err := NewMarketClientWithURL(srv.URL).GetMarketEvents(context.Background(), "3988550")
	if err != nil {
		t.Fatalf("empty list must not error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

// A list-form element that carries no events[] key (element present but without
// a parent event) is also empty-and-nil, not an error.
func TestGetMarketEventsElementWithoutEventsIsNotAnError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":"3988550","question":"Will X win?"}]`)
	}))
	t.Cleanup(srv.Close)

	events, err := NewMarketClientWithURL(srv.URL).GetMarketEvents(context.Background(), "3988550")
	if err != nil {
		t.Fatalf("element without events must not error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

// Only a genuine transport/HTTP failure is an error — the caller logs the bail
// and keeps the un-enriched market.
func TestGetMarketEventsHTTPErrorPropagates(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	if _, err := NewMarketClientWithURL(srv.URL).GetMarketEvents(context.Background(), "3682196"); err == nil {
		t.Fatal("want an error on HTTP 500")
	}
}
