package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Catorpilor/poly/internal/config"
	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
)

// The Live Watch web endpoints (ADR 0008 phase 3) are session-validated
// exactly like /api/trade and funnel through the SAME manager methods as
// /live, so a web-created watch and a Telegram-created one are one object.
// These tests drive the HTTP handlers with a fake manager (SubscribeTelegram
// otherwise resolves against Gamma) and a fake user repo.

// fakeWatchManager is an in-memory stand-in for *LiveTradeManager's Live
// Watch surface: it records the last SubscribeTelegram call and tracks the
// per-user (slug -> tape) set so the handlers' cap/list/delete logic is
// exercised end to end without Gamma or RTDS.
type fakeWatchManager struct {
	mu             sync.Mutex
	subs           map[int64]map[string]bool // chatID -> slug -> tape
	title          string
	subErr         error
	subscribeCalls int
	lastChatID     int64
	lastSlug       string
	lastTape       bool
}

func newFakeWatchManager() *fakeWatchManager {
	return &fakeWatchManager{subs: map[int64]map[string]bool{}, title: "Test Event"}
}

func (f *fakeWatchManager) SubscribeTelegram(_ context.Context, chatID int64, slug string, tape bool) (*EventInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribeCalls++
	f.lastChatID, f.lastSlug, f.lastTape = chatID, slug, tape
	if f.subErr != nil {
		return nil, f.subErr
	}
	if f.subs[chatID] == nil {
		f.subs[chatID] = map[string]bool{}
	}
	f.subs[chatID][slug] = tape
	return &EventInfo{Slug: slug, Title: f.title}, nil
}

func (f *fakeWatchManager) UnsubscribeTelegram(chatID int64, slug string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.subs[chatID][slug]; !ok {
		return false
	}
	delete(f.subs[chatID], slug)
	return true
}

func (f *fakeWatchManager) GetUserSubscriptions(chatID int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.subs[chatID]))
	for slug := range f.subs[chatID] {
		out = append(out, slug)
	}
	return out
}

func (f *fakeWatchManager) IsTapeSubscription(chatID int64, slug string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs[chatID][slug]
}

// tapeOf reads back the stored tape flag for assertions.
func (f *fakeWatchManager) tapeOf(chatID int64, slug string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs[chatID][slug]
}

// seed pre-populates a subscription without going through HTTP.
func (f *fakeWatchManager) seed(chatID int64, slug string, tape bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs[chatID] == nil {
		f.subs[chatID] = map[string]bool{}
	}
	f.subs[chatID][slug] = tape
}

// fakeSubUserRepo serves one fixed user for every telegram ID; nil user means
// "no such user" (the repo returns nil, nil on a missing row).
type fakeSubUserRepo struct {
	repositories.UserRepository
	user *database.User
}

func (f *fakeSubUserRepo) GetByTelegramID(context.Context, int64) (*database.User, error) {
	return f.user, nil
}

const (
	subTestChatID       = int64(42)
	subTestProxyAddress = "0xproxy"
)

func subTestUser() *database.User {
	return &database.User{TelegramID: subTestChatID, ProxyAddress: subTestProxyAddress}
}

// newSubTestServer builds a guarded web server wired to fakes.
func newSubTestServer(t *testing.T, watches liveWatchManager, user *database.User) *WebServer {
	t.Helper()
	cfg := &config.Config{}
	cfg.App.LiveWebURL = "http://localhost:8081"
	ws := NewWebServer(nil, 0, nil, cfg, nil, nil)
	ws.watches = watches
	ws.userRepo = &fakeSubUserRepo{user: user}
	return ws
}

// subReq issues a request through the full handler chain (guard included).
func subReq(t *testing.T, ws *WebServer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://localhost:8081"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ws.httpServer.Handler.ServeHTTP(rec, req)
	return rec
}

// authedSession is a request body session block for the fixed test user.
const authedSession = `"session":{"telegramId":42,"walletAddress":"0xeoa","proxyAddress":"0xproxy"}`

// All three endpoints reject an unauthenticated session (no TelegramID) with
// 401 before touching the manager — strictly no weaker than /api/trade.
func TestSubscriptionEndpointsRequireAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"put", http.MethodPut, "/api/events/nba-lal-por-2026-01-17/subscription", `{"session":{},"tape":true}`},
		{"delete", http.MethodDelete, "/api/events/nba-lal-por-2026-01-17/subscription", `{"session":{}}`},
		{"list", http.MethodPost, "/api/subscriptions/list", `{"session":{}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeWatchManager()
			ws := newSubTestServer(t, fake, subTestUser())

			rec := subReq(t, ws, tt.method, tt.path, tt.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d, want 401; body: %s", tt.method, tt.path, rec.Code, rec.Body.String())
			}
			fake.mu.Lock()
			calls := fake.subscribeCalls
			fake.mu.Unlock()
			if calls != 0 {
				t.Errorf("manager touched on an unauthenticated request (subscribeCalls=%d)", calls)
			}
		})
	}
}

// A proxy-address mismatch is a 401, mirroring /api/trade's wallet check.
func TestSubscriptionRejectsProxyMismatch(t *testing.T) {
	t.Parallel()
	fake := newFakeWatchManager()
	ws := newSubTestServer(t, fake, subTestUser())

	body := `{"session":{"telegramId":42,"proxyAddress":"0xWRONG"},"tape":false}`
	rec := subReq(t, ws, http.MethodPut, "/api/events/evt/subscription", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("proxy mismatch = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

// PUT creates a Live Watch: the manager receives the right chatID/slug/tape
// and the response carries the resolved title and the stored tape flag.
func TestPutSubscriptionCreates(t *testing.T) {
	t.Parallel()
	fake := newFakeWatchManager()
	fake.title = "Lakers vs Blazers"
	ws := newSubTestServer(t, fake, subTestUser())

	slug := "nba-lal-por-2026-01-17"
	body := fmt.Sprintf(`{%s,"tape":false}`, authedSession)
	rec := subReq(t, ws, http.MethodPut, "/api/events/"+slug+"/subscription", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	if fake.lastChatID != subTestChatID || fake.lastSlug != slug || fake.lastTape != false {
		t.Errorf("manager got (chat=%d slug=%q tape=%v), want (42, %q, false)",
			fake.lastChatID, fake.lastSlug, fake.lastTape, slug)
	}

	var resp struct {
		Success    bool   `json:"success"`
		EventTitle string `json:"eventTitle"`
		Tape       bool   `json:"tape"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || resp.EventTitle != "Lakers vs Blazers" || resp.Tape != false {
		t.Errorf("response = %+v, want success/title/tape=false", resp)
	}
}

// PUT with tape=true stores the tape flag.
func TestPutSubscriptionTapeTrue(t *testing.T) {
	t.Parallel()
	fake := newFakeWatchManager()
	ws := newSubTestServer(t, fake, subTestUser())

	slug := "evt"
	body := fmt.Sprintf(`{%s,"tape":true}`, authedSession)
	rec := subReq(t, ws, http.MethodPut, "/api/events/"+slug+"/subscription", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT tape=true = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !fake.tapeOf(subTestChatID, slug) {
		t.Error("tape flag not stored on the watch")
	}
}

// Re-PUT with a different tape flips the flag on the same watch — one
// distinct event, two subscribe calls.
func TestPutSubscriptionTapeFlip(t *testing.T) {
	t.Parallel()
	fake := newFakeWatchManager()
	ws := newSubTestServer(t, fake, subTestUser())
	slug := "evt"

	if rec := subReq(t, ws, http.MethodPut, "/api/events/"+slug+"/subscription",
		fmt.Sprintf(`{%s,"tape":false}`, authedSession)); rec.Code != http.StatusOK {
		t.Fatalf("first PUT = %d, want 200", rec.Code)
	}
	if fake.tapeOf(subTestChatID, slug) {
		t.Fatal("tape should be off after first PUT")
	}

	if rec := subReq(t, ws, http.MethodPut, "/api/events/"+slug+"/subscription",
		fmt.Sprintf(`{%s,"tape":true}`, authedSession)); rec.Code != http.StatusOK {
		t.Fatalf("re-PUT = %d, want 200", rec.Code)
	}
	if !fake.tapeOf(subTestChatID, slug) {
		t.Error("tape should be on after re-PUT")
	}
	if got := fake.GetUserSubscriptions(subTestChatID); len(got) != 1 {
		t.Errorf("distinct events = %d, want 1 (re-PUT must not duplicate)", len(got))
	}
	if fake.subscribeCalls != 2 {
		t.Errorf("subscribeCalls = %d, want 2", fake.subscribeCalls)
	}
}

// DELETE removes a watch; deleting an absent watch is a 404.
func TestDeleteSubscription(t *testing.T) {
	t.Parallel()
	fake := newFakeWatchManager()
	fake.seed(subTestChatID, "evt", false)
	ws := newSubTestServer(t, fake, subTestUser())

	rec := subReq(t, ws, http.MethodDelete, "/api/events/evt/subscription",
		fmt.Sprintf(`{%s}`, authedSession))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(fake.GetUserSubscriptions(subTestChatID)) != 0 {
		t.Error("watch not removed")
	}

	rec = subReq(t, ws, http.MethodDelete, "/api/events/evt/subscription",
		fmt.Sprintf(`{%s}`, authedSession))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE absent = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
}

// The list endpoint returns each watch's slug and tape flag.
func TestListSubscriptions(t *testing.T) {
	t.Parallel()
	fake := newFakeWatchManager()
	fake.seed(subTestChatID, "evt-quiet", false)
	fake.seed(subTestChatID, "evt-tape", true)
	ws := newSubTestServer(t, fake, subTestUser())

	rec := subReq(t, ws, http.MethodPost, "/api/subscriptions/list", fmt.Sprintf(`{%s}`, authedSession))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var items []struct {
		EventSlug string `json:"eventSlug"`
		Tape      bool   `json:"tape"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	got := map[string]bool{}
	for _, it := range items {
		got[it.EventSlug] = it.Tape
	}
	if len(got) != 2 || got["evt-quiet"] != false || got["evt-tape"] != true {
		t.Errorf("list = %v, want {evt-quiet:false, evt-tape:true}", got)
	}
}

// The 31st distinct watch is refused with 409; a re-PUT of an existing watch
// at the cap still succeeds (it is not a new distinct event).
func TestPutSubscriptionWatchCap(t *testing.T) {
	t.Parallel()
	fake := newFakeWatchManager()
	for i := 0; i < maxLiveWatchesPerUser; i++ {
		fake.seed(subTestChatID, fmt.Sprintf("evt-%d", i), false)
	}
	ws := newSubTestServer(t, fake, subTestUser())

	// 31st distinct event → 409.
	rec := subReq(t, ws, http.MethodPut, "/api/events/evt-new/subscription",
		fmt.Sprintf(`{%s,"tape":false}`, authedSession))
	if rec.Code != http.StatusConflict {
		t.Fatalf("31st PUT = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "watch limit reached (30)") {
		t.Errorf("409 body = %q, want the limit message", rec.Body.String())
	}

	// Re-PUT of an existing watch at the cap → still 200 (tape flip allowed).
	rec = subReq(t, ws, http.MethodPut, "/api/events/evt-0/subscription",
		fmt.Sprintf(`{%s,"tape":true}`, authedSession))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-PUT at cap = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !fake.tapeOf(subTestChatID, "evt-0") {
		t.Error("re-PUT at cap should have flipped the tape flag")
	}
}
