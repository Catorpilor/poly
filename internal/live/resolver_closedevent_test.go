package live

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// closedEventJSON is a Gamma /events?closed=true response body for one closed
// event whose markets are all closed=true.
func closedEventJSON(slug string) string {
	return `[{"id":"e","slug":"` + slug + `","title":"AA vs BB",` +
		`"markets":[` +
		`{"id":"m1","question":"AA vs BB","closed":true,"active":false},` +
		`{"id":"m2","question":"AA vs BB - Game 2","closed":true,"active":false}` +
		`]}]`
}

// TestClosedEventBySlug_ReturnsClosedEvent: the closed=true filter returns the
// identity-matched event, and every nested market carries closed=true.
func TestClosedEventBySlug_ReturnsClosedEvent(t *testing.T) {
	t.Parallel()
	const slug = "aa-vs-bb-2026-08-08"
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "closed=true") {
			t.Errorf("closed=true filter missing from query: %s", r.URL.RawQuery)
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, closedEventJSON(slug))
	}))
	defer srv.Close()

	r := NewEventSlugResolver()
	r.gammaAPIURL = srv.URL

	event, err := r.ClosedEventBySlug(context.Background(), slug)
	if err != nil {
		t.Fatalf("ClosedEventBySlug: %v", err)
	}
	if event.Slug != slug {
		t.Errorf("event slug = %q, want %q", event.Slug, slug)
	}
	if !allMarketsClosed(event) {
		t.Error("every market should be closed=true")
	}
}

// TestClosedEventBySlug_ActiveEmptyIsNotClosed: an active event is excluded by
// the closed=true filter (empty list) — the common negative, surfaced as
// ErrEventNotClosed so the sweep keeps the watch quietly.
func TestClosedEventBySlug_ActiveEmptyIsNotClosed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, `[]`)
	}))
	defer srv.Close()

	r := NewEventSlugResolver()
	r.gammaAPIURL = srv.URL

	_, err := r.ClosedEventBySlug(context.Background(), "still-live")
	if !errors.Is(err, ErrEventNotClosed) {
		t.Errorf("err = %v, want ErrEventNotClosed", err)
	}
}

// TestClosedEventBySlug_IdentityMismatch: if Gamma ignores the slug filter and
// returns a default list of closed events (#33 trap), the mismatch must be a
// hard error — never passed off as this event.
func TestClosedEventBySlug_IdentityMismatch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, closedEventJSON("some-other-event"))
	}))
	defer srv.Close()

	r := NewEventSlugResolver()
	r.gammaAPIURL = srv.URL

	_, err := r.ClosedEventBySlug(context.Background(), "requested-event")
	if err == nil {
		t.Fatal("identity mismatch must be an error")
	}
	if errors.Is(err, ErrEventNotClosed) {
		t.Error("identity mismatch must be a hard error, not the not-closed negative")
	}
}

// TestClosedEventBySlug_ServerError: a non-200 propagates as a real error (kept
// + logged by the sweep), never as ErrEventNotClosed.
func TestClosedEventBySlug_ServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewEventSlugResolver()
	r.gammaAPIURL = srv.URL

	_, err := r.ClosedEventBySlug(context.Background(), "evt")
	if err == nil {
		t.Fatal("server error must propagate")
	}
	if errors.Is(err, ErrEventNotClosed) {
		t.Error("server error must not be reported as the not-closed negative")
	}
}

// TestClosedEventBySlug_DoesNotPolluteCache: the closed lookup must not seed the
// resolver's event cache — otherwise GetEventInfo would start returning a closed
// event to the trade feed / refresh paths.
func TestClosedEventBySlug_DoesNotPolluteCache(t *testing.T) {
	t.Parallel()
	const slug = "cache-check-evt"
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, closedEventJSON(slug))
	}))
	defer srv.Close()

	r := NewEventSlugResolver()
	r.gammaAPIURL = srv.URL

	if _, err := r.ClosedEventBySlug(context.Background(), slug); err != nil {
		t.Fatalf("ClosedEventBySlug: %v", err)
	}
	r.mu.RLock()
	_, cached := r.cache[slug]
	r.mu.RUnlock()
	if cached {
		t.Error("closed lookup polluted the resolver cache")
	}
}
