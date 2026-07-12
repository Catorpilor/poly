package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Users paste market slugs where event slugs are expected (a Polymarket
// market page URL ends in the market slug, e.g. …-game1). GetEventInfo
// falls back to a market-by-slug lookup and follows it to the parent
// event instead of failing.
func TestGetEventInfoResolvesMarketSlugToParentEvent(t *testing.T) {
	t.Parallel()

	const eventSlug = "lol-blg-hle1-2026-07-12"
	const marketSlug = "lol-blg-hle1-2026-07-12-game1"

	var gammaHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gammaHits.Add(1)
		slug := r.URL.Query().Get("slug")
		switch {
		case strings.HasPrefix(r.URL.Path, "/events"):
			if slug == eventSlug {
				json.NewEncoder(w).Encode([]EventInfo{{ID: "e1", Slug: eventSlug, Title: "BLG vs HLE (BO5)"}})
				return
			}
			json.NewEncoder(w).Encode([]EventInfo{})
		case strings.HasPrefix(r.URL.Path, "/markets"):
			if slug == marketSlug {
				json.NewEncoder(w).Encode([]map[string]interface{}{{
					"question": "Game 1 Winner",
					"events":   []map[string]string{{"slug": eventSlug}},
				}})
				return
			}
			json.NewEncoder(w).Encode([]map[string]interface{}{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	r := NewEventSlugResolver()
	r.gammaAPIURL = srv.URL

	info, err := r.GetEventInfo(context.Background(), marketSlug)
	if err != nil {
		t.Fatalf("GetEventInfo(%s) = %v, want parent event", marketSlug, err)
	}
	if info.Slug != eventSlug {
		t.Fatalf("resolved slug = %q, want parent event %q", info.Slug, eventSlug)
	}

	// The result is cached under the input slug too: a repeat lookup must
	// not hit Gamma again.
	before := gammaHits.Load()
	if _, err := r.GetEventInfo(context.Background(), marketSlug); err != nil {
		t.Fatalf("cached lookup: %v", err)
	}
	if gammaHits.Load() != before {
		t.Errorf("repeat lookup hit Gamma %d more time(s), want cache hit", gammaHits.Load()-before)
	}

	// An event slug still resolves directly (no behavior change).
	if info, err := r.GetEventInfo(context.Background(), eventSlug); err != nil || info.Slug != eventSlug {
		t.Errorf("GetEventInfo(%s) = (%v, %v), want direct hit", eventSlug, info, err)
	}
}

func TestGetEventInfoUnknownSlugStillErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/events") || strings.HasPrefix(r.URL.Path, "/markets") {
			w.Write([]byte("[]"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	r := NewEventSlugResolver()
	r.gammaAPIURL = srv.URL

	_, err := r.GetEventInfo(context.Background(), "totally-bogus")
	if err == nil || !strings.Contains(err.Error(), "totally-bogus") {
		t.Fatalf("err = %v, want event-not-found naming the input slug", err)
	}
}
