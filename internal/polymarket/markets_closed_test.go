package polymarket

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestGetClosedMarketByConditionID covers the closed-only lookup used by the
// resolved-arm sweeper (issue #39). Live-verified Gamma semantics:
// /markets?condition_ids=<id>&closed=true returns the market iff it is
// actually closed; open or just-finished-but-unresolved markets return an
// empty list. Gamma also silently ignores unknown params and serves its
// default market list (the #33/#38 saga), so the response's conditionId must
// match the request — the "ignore-and-default" case models that failure.
func TestGetClosedMarketByConditionID(t *testing.T) {
	t.Parallel()

	const conditionID = "0xe332000000000000000000000000000000000000000000000000000000000001"

	tests := []struct {
		name string
		body string
		// wantSlug is the expected market slug on success; empty = error case.
		wantSlug string
		// wantErr is a substring the error must contain.
		wantErr string
		// wantNotFound asserts errors.Is(err, ErrMarketNotFound).
		wantNotFound bool
	}{
		{
			name: "closed market returned",
			body: `[{"id":"501","question":"VAL: GM vs NAVI map 1","conditionId":"` +
				conditionID + `","slug":"val-gm-navi1","closed":true}]`,
			wantSlug: "val-gm-navi1",
		},
		{
			name: "conditionId matched case-insensitively",
			body: `[{"id":"501","question":"VAL: GM vs NAVI map 1","conditionId":"` +
				strings.ToUpper(conditionID) + `","slug":"val-gm-navi1","closed":true}]`,
			wantSlug: "val-gm-navi1",
		},
		{
			name:         "empty response is not-found (market open or unresolved)",
			body:         `[]`,
			wantErr:      "market not found",
			wantNotFound: true,
		},
		{
			name:    "ignored filter rejected by identity check",
			body:    `[{"id":"9999","question":"Unrelated","conditionId":"0xdead","slug":"xi-jinping-out","closed":true}]`,
			wantErr: "filter not applied",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotQuery url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.Query()
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			mc := NewMarketClientWithURL(srv.URL)
			market, err := mc.GetClosedMarketByConditionID(context.Background(), conditionID)

			// Every variant of the closed lookup must send both params.
			if got := gotQuery.Get("condition_ids"); got != conditionID {
				t.Errorf("condition_ids param = %q, want %q", got, conditionID)
			}
			if got := gotQuery.Get("closed"); got != "true" {
				t.Errorf("closed param = %q, want %q", got, "true")
			}

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got market %+v", tt.wantErr, market)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err, tt.wantErr)
				}
				if tt.wantNotFound && !errors.Is(err, ErrMarketNotFound) {
					t.Errorf("error %q is not ErrMarketNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetClosedMarketByConditionID: %v", err)
			}
			if market.Slug != tt.wantSlug {
				t.Errorf("slug = %q, want %q", market.Slug, tt.wantSlug)
			}
			if !market.Closed {
				t.Error("returned market should have Closed=true")
			}
		})
	}
}

// TestGetMarketByConditionIDOmitsClosedParam locks in that the plain lookup
// stays unfiltered: it must not inherit the sweeper's closed=true param.
func TestGetMarketByConditionIDOmitsClosedParam(t *testing.T) {
	t.Parallel()

	const conditionID = "0xe332000000000000000000000000000000000000000000000000000000000002"

	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		fmt.Fprintf(w, `[{"id":"7","question":"Open market","conditionId":"%s","slug":"open-market"}]`, conditionID)
	}))
	defer srv.Close()

	mc := NewMarketClientWithURL(srv.URL)
	if _, err := mc.GetMarketByConditionID(context.Background(), conditionID); err != nil {
		t.Fatalf("GetMarketByConditionID: %v", err)
	}
	if _, present := gotQuery["closed"]; present {
		t.Errorf("plain lookup sent closed=%q; must omit the param", gotQuery.Get("closed"))
	}

	// The empty-response error carries the sentinel on the plain path too.
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	defer srvEmpty.Close()
	mcEmpty := NewMarketClientWithURL(srvEmpty.URL)
	_, err := mcEmpty.GetMarketByConditionID(context.Background(), conditionID)
	if !errors.Is(err, ErrMarketNotFound) {
		t.Errorf("empty plain lookup error %v is not ErrMarketNotFound", err)
	}
}
