package telegram

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Catorpilor/poly/internal/config"
	"github.com/Catorpilor/poly/internal/live"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// fakeLiveSubs is an in-memory liveSubscriptions for /live and /stoplive
// fan-out tests (issue #90). It records subscribe/unsubscribe calls so a test
// can prove one row per member, the issuer's personal tape, and preserved
// member tape — with no Polymarket resolve or WebSocket. failMember[id] makes
// that member's SubscribeTelegram error, driving the partial-failure path.
type fakeLiveSubs struct {
	mu         sync.Mutex
	subs       map[int64]map[string]bool // chatID -> slug -> tape
	subCalls   []subCall
	unsubCalls []unsubCall
	unsubAll   []int64
	title      string
	markets    int
	failMember map[int64]error
}

type subCall struct {
	chatID int64
	slug   string
	tape   bool
}

type unsubCall struct {
	chatID int64
	slug   string
}

func newFakeLiveSubs() *fakeLiveSubs {
	return &fakeLiveSubs{
		subs:       map[int64]map[string]bool{},
		title:      "Test Event",
		markets:    2,
		failMember: map[int64]error{},
	}
}

// seed pre-subscribes chatID to slug at the given tape (models an existing
// member subscription for the preserve-tape case).
func (f *fakeLiveSubs) seed(chatID int64, slug string, tape bool) {
	if f.subs[chatID] == nil {
		f.subs[chatID] = map[string]bool{}
	}
	f.subs[chatID][slug] = tape
}

func (f *fakeLiveSubs) SubscribeTelegram(_ context.Context, chatID int64, slug string, tape bool) (*live.EventInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failMember[chatID]; err != nil {
		return nil, err
	}
	f.subCalls = append(f.subCalls, subCall{chatID, slug, tape})
	if f.subs[chatID] == nil {
		f.subs[chatID] = map[string]bool{}
	}
	f.subs[chatID][slug] = tape
	return &live.EventInfo{Title: f.title, Slug: slug, Markets: make([]live.MarketInfo, f.markets)}, nil
}

func (f *fakeLiveSubs) IsTapeSubscription(chatID int64, slug string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs[chatID][slug]
}

func (f *fakeLiveSubs) UnsubscribeTelegram(chatID int64, slug string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubCalls = append(f.unsubCalls, unsubCall{chatID, slug})
	if f.subs[chatID] == nil {
		return false
	}
	if _, ok := f.subs[chatID][slug]; !ok {
		return false
	}
	delete(f.subs[chatID], slug)
	return true
}

func (f *fakeLiveSubs) UnsubscribeAllTelegram(chatID int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubAll = append(f.unsubAll, chatID)
	var out []string
	for slug := range f.subs[chatID] {
		out = append(out, slug)
	}
	delete(f.subs, chatID)
	return out
}

func (f *fakeLiveSubs) GetUserSubscriptions(chatID int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for slug := range f.subs[chatID] {
		out = append(out, slug)
	}
	return out
}

func (f *fakeLiveSubs) subCallFor(chatID int64) (subCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.subCalls {
		if c.chatID == chatID {
			return c, true
		}
	}
	return subCall{}, false
}

func (f *fakeLiveSubs) hasUnsub(chatID int64, slug string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.unsubCalls {
		if c.chatID == chatID && c.slug == slug {
			return true
		}
	}
	return false
}

func (f *fakeLiveSubs) hasUnsubAll(chatID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.unsubAll {
		if c == chatID {
			return true
		}
	}
	return false
}

// newFanoutBot wires a Bot with a recording Telegram server and the given
// household + fake subscription manager.
func newFanoutBot(t *testing.T, members []int64, subs *fakeLiveSubs) (*Bot, *tgRecorder) {
	t.Helper()
	tg := &tgRecorder{}
	srv := httptest.NewServer(tg)
	t.Cleanup(srv.Close)
	api, err := tgbotapi.NewBotAPIWithClient("test-token", srv.URL+"/bot%s/%s", srv.Client())
	if err != nil {
		t.Fatalf("NewBotAPIWithClient: %v", err)
	}
	b := &Bot{
		api:      api,
		liveSubs: subs,
		config:   &config.Config{Telegram: config.TelegramConfig{LinkedChatIDs: members}},
	}
	return b, tg
}

// cmdUpdate builds a command Update whose CommandArguments() yields args.
func cmdUpdate(chatID int64, username, cmd, args string) *tgbotapi.Update {
	text := cmd
	if args != "" {
		text = cmd + " " + args
	}
	from := &tgbotapi.User{ID: chatID}
	if username != "" {
		from.UserName = username
	}
	return &tgbotapi.Update{Message: &tgbotapi.Message{
		Chat:     &tgbotapi.Chat{ID: chatID},
		From:     from,
		Text:     text,
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: len(cmd)}},
	}}
}

// sendsTo returns the recorded sendMessage payloads addressed to chatID.
func sendsTo(tg *tgRecorder, chatID int64) []tgMessage {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	want := strconv.FormatInt(chatID, 10)
	var out []tgMessage
	for _, m := range tg.sends {
		if m.chatID == want {
			out = append(out, m)
		}
	}
	return out
}

func mustSendTo(t *testing.T, tg *tgRecorder, chatID int64) tgMessage {
	t.Helper()
	got := sendsTo(tg, chatID)
	if len(got) == 0 {
		t.Fatalf("no message sent to chat %d", chatID)
	}
	return got[len(got)-1]
}

// --- pure helpers ---------------------------------------------------------

func TestIsHouseholdMember(t *testing.T) {
	t.Parallel()
	members := []int64{1, 2, 3}
	if !isHouseholdMember(2, members) {
		t.Error("2 should be a member")
	}
	if isHouseholdMember(9, members) {
		t.Error("9 should not be a member")
	}
	if isHouseholdMember(1, nil) {
		t.Error("empty household has no members")
	}
}

func TestOtherMembers(t *testing.T) {
	t.Parallel()
	got := otherMembers(2, []int64{1, 2, 3, 2, 1})
	want := []int64{1, 3}
	if len(got) != len(want) {
		t.Fatalf("otherMembers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("otherMembers = %v, want %v", got, want)
		}
	}
}

func TestIssuerLabel(t *testing.T) {
	t.Parallel()
	if got := issuerLabel(&tgbotapi.User{UserName: "zeh"}, 7); got != "@zeh" {
		t.Errorf("username label = %q", got)
	}
	if got := issuerLabel(&tgbotapi.User{FirstName: "Ada", LastName: "Lovelace"}, 7); got != "Ada Lovelace" {
		t.Errorf("name label = %q", got)
	}
	if got := issuerLabel(nil, 7); got != "account 7" {
		t.Errorf("fallback label = %q", got)
	}
}

func TestFanoutConfirmSuffix(t *testing.T) {
	t.Parallel()
	if got := fanoutConfirmSuffix(0); got != "" {
		t.Errorf("zero suffix = %q, want empty", got)
	}
	if got := fanoutConfirmSuffix(3); !strings.Contains(got, "3 linked account") {
		t.Errorf("suffix = %q", got)
	}
}

func TestFanoutNoticeCopy(t *testing.T) {
	t.Parallel()
	sub := fanoutSubscribeNotice("@zeh", "nba-lal-por-2026-01-17")
	for _, want := range []string{"@zeh", "subscribed this household to", "nba-lal-por-2026-01-17", "full recipient", "snipe auto-buy applies", "/stoplive nba-lal-por-2026-01-17 to opt out"} {
		if !strings.Contains(sub, want) {
			t.Errorf("subscribe notice missing %q in %q", want, sub)
		}
	}
	unsub := fanoutUnsubscribeNotice("@zeh", "nba-lal-por-2026-01-17")
	for _, want := range []string{"@zeh", "unsubscribed this household from", "no longer a recipient"} {
		if !strings.Contains(unsub, want) {
			t.Errorf("unsubscribe notice missing %q in %q", want, unsub)
		}
	}
	all := fanoutStopAllNotice("@zeh", []string{"a", "b"})
	for _, want := range []string{"@zeh", "stoplive all", "ALL", "a", "b"} {
		if !strings.Contains(all, want) {
			t.Errorf("stop-all notice missing %q in %q", want, all)
		}
	}
}

// --- /live fan-out --------------------------------------------------------

const fanoutSlug = "nba-lal-por-2026-01-17"

// TestHandleLiveFanoutSubscribesEveryMember: a member's /live subscribes every
// member (one row each), honors the issuer's tape, keeps other members quiet on
// create, preserves an existing member's tape, DMs every other member, and
// notes the fan-out count on the issuer's confirmation.
func TestHandleLiveFanoutSubscribesEveryMember(t *testing.T) {
	t.Parallel()
	members := []int64{10, 20, 30}
	subs := newFakeLiveSubs()
	subs.seed(30, fanoutSlug, true) // member 30 already chose tape
	b, tg := newFanoutBot(t, members, subs)

	// issuer 10 subscribes with tape.
	if err := b.handleLive(context.Background(), b, cmdUpdate(10, "zeh", "/live", fanoutSlug+" tape")); err != nil {
		t.Fatalf("handleLive: %v", err)
	}

	// One subscribe per member.
	if len(subs.subCalls) != 3 {
		t.Fatalf("subscribe calls = %d, want 3 (one per member): %+v", len(subs.subCalls), subs.subCalls)
	}
	// Issuer keeps its own tape flag (true).
	if c, ok := subs.subCallFor(10); !ok || !c.tape {
		t.Errorf("issuer sub = %+v, ok=%v, want tape=true", c, ok)
	}
	// Fresh member is quiet on create.
	if c, ok := subs.subCallFor(20); !ok || c.tape {
		t.Errorf("member 20 sub = %+v, ok=%v, want tape=false (quiet)", c, ok)
	}
	// Existing tape member keeps tape (never downgraded).
	if c, ok := subs.subCallFor(30); !ok || !c.tape {
		t.Errorf("member 30 sub = %+v, ok=%v, want tape=true (preserved)", c, ok)
	}

	// Every OTHER member gets the fan-out DM.
	for _, m := range []int64{20, 30} {
		msg := mustSendTo(t, tg, m)
		if !strings.Contains(msg.text, "@zeh") || !strings.Contains(msg.text, "full recipient") || !strings.Contains(msg.text, fanoutSlug) {
			t.Errorf("member %d DM missing fan-out copy: %q", m, msg.text)
		}
	}
	// Issuer gets confirmation noting the fan-out count.
	conf := mustSendTo(t, tg, 10)
	if !strings.Contains(conf.text, "2 linked account") {
		t.Errorf("issuer confirmation missing fan-out count: %q", conf.text)
	}
}

// TestHandleLivePartialFailure: one member's subscribe errors — the others are
// still processed, the failed member gets no DM, and the issuer is told which
// member failed (never silent).
func TestHandleLivePartialFailure(t *testing.T) {
	t.Parallel()
	members := []int64{10, 20, 30}
	subs := newFakeLiveSubs()
	subs.failMember[20] = errors.New("resolve boom")
	b, tg := newFanoutBot(t, members, subs)

	if err := b.handleLive(context.Background(), b, cmdUpdate(10, "zeh", "/live", fanoutSlug)); err != nil {
		t.Fatalf("handleLive: %v", err)
	}

	// 30 still subscribed despite 20 failing.
	if _, ok := subs.subCallFor(30); !ok {
		t.Error("member 30 should still be subscribed after member 20 failed")
	}
	// Failed member gets no DM.
	if got := sendsTo(tg, 20); len(got) != 0 {
		t.Errorf("failed member 20 should get no DM, got %d", len(got))
	}
	// Issuer told which member failed.
	conf := mustSendTo(t, tg, 10)
	if !strings.Contains(conf.text, "20") {
		t.Errorf("issuer confirmation should name failed member 20: %q", conf.text)
	}
	if !strings.Contains(conf.text, "1 linked account") {
		t.Errorf("issuer confirmation should note the 1 successful fan-out: %q", conf.text)
	}
}

// TestHandleLiveNonMemberNoFanout: a non-member's /live behaves exactly as
// today — only the issuer subscribes, no DMs, no fan-out note.
func TestHandleLiveNonMemberNoFanout(t *testing.T) {
	t.Parallel()
	members := []int64{10, 20, 30}
	subs := newFakeLiveSubs()
	b, tg := newFanoutBot(t, members, subs)

	if err := b.handleLive(context.Background(), b, cmdUpdate(99, "outsider", "/live", fanoutSlug)); err != nil {
		t.Fatalf("handleLive: %v", err)
	}
	if len(subs.subCalls) != 1 {
		t.Fatalf("non-member subscribe calls = %d, want 1", len(subs.subCalls))
	}
	if tg.sendCount() != 1 {
		t.Fatalf("non-member should send only its own confirmation, got %d", tg.sendCount())
	}
	conf := mustSendTo(t, tg, 99)
	if strings.Contains(conf.text, "linked account") {
		t.Errorf("non-member confirmation must not mention fan-out: %q", conf.text)
	}
}

// TestHandleLiveFeatureOff: empty household ⇒ bit-identical single-account
// behavior even for a chat that would otherwise be an issuer.
func TestHandleLiveFeatureOff(t *testing.T) {
	t.Parallel()
	subs := newFakeLiveSubs()
	b, tg := newFanoutBot(t, nil, subs)

	if err := b.handleLive(context.Background(), b, cmdUpdate(10, "zeh", "/live", fanoutSlug)); err != nil {
		t.Fatalf("handleLive: %v", err)
	}
	if len(subs.subCalls) != 1 {
		t.Fatalf("feature-off subscribe calls = %d, want 1", len(subs.subCalls))
	}
	if tg.sendCount() != 1 {
		t.Fatalf("feature-off should send only the issuer confirmation, got %d", tg.sendCount())
	}
	if strings.Contains(mustSendTo(t, tg, 10).text, "linked account") {
		t.Error("feature-off confirmation must not mention fan-out")
	}
}

// --- /stoplive fan-out ----------------------------------------------------

// TestHandleStopLiveSlugFanout: a member's /stoplive <slug> unsubscribes every
// member from that slug and DMs the others the mirror notice.
func TestHandleStopLiveSlugFanout(t *testing.T) {
	t.Parallel()
	members := []int64{10, 20, 30}
	subs := newFakeLiveSubs()
	for _, m := range members {
		subs.seed(m, fanoutSlug, false)
	}
	b, tg := newFanoutBot(t, members, subs)

	if err := b.handleStopLive(context.Background(), b, cmdUpdate(10, "zeh", "/stoplive", fanoutSlug)); err != nil {
		t.Fatalf("handleStopLive: %v", err)
	}
	for _, m := range members {
		if !subs.hasUnsub(m, fanoutSlug) {
			t.Errorf("member %d not unsubscribed from %s", m, fanoutSlug)
		}
	}
	for _, m := range []int64{20, 30} {
		msg := mustSendTo(t, tg, m)
		if !strings.Contains(msg.text, "no longer a recipient") || !strings.Contains(msg.text, fanoutSlug) {
			t.Errorf("member %d mirror DM missing copy: %q", m, msg.text)
		}
	}
	conf := mustSendTo(t, tg, 10)
	if !strings.Contains(conf.text, "2 linked account") {
		t.Errorf("issuer confirmation missing fan-out count: %q", conf.text)
	}
}

// TestHandleStopLiveAllFanout: a member's /stoplive all clears every member's
// full subscription set and DMs the others the mirror.
func TestHandleStopLiveAllFanout(t *testing.T) {
	t.Parallel()
	members := []int64{10, 20, 30}
	subs := newFakeLiveSubs()
	for _, m := range members {
		subs.seed(m, fanoutSlug, false)
		subs.seed(m, "epl-ars-che-2026-02-01", false)
	}
	b, tg := newFanoutBot(t, members, subs)

	if err := b.handleStopLive(context.Background(), b, cmdUpdate(10, "zeh", "/stoplive", "all")); err != nil {
		t.Fatalf("handleStopLive: %v", err)
	}
	for _, m := range members {
		if !subs.hasUnsubAll(m) {
			t.Errorf("member %d full set not cleared", m)
		}
	}
	for _, m := range []int64{20, 30} {
		msg := mustSendTo(t, tg, m)
		if !strings.Contains(msg.text, "stoplive all") {
			t.Errorf("member %d stop-all mirror missing copy: %q", m, msg.text)
		}
	}
	conf := mustSendTo(t, tg, 10)
	if !strings.Contains(conf.text, "2 linked account") {
		t.Errorf("issuer stop-all confirmation missing fan-out count: %q", conf.text)
	}
}

// TestHandleStopLiveNonMember: a non-member's /stoplive is unchanged — only the
// issuer is unsubscribed, no DMs.
func TestHandleStopLiveNonMember(t *testing.T) {
	t.Parallel()
	members := []int64{10, 20, 30}
	subs := newFakeLiveSubs()
	subs.seed(99, fanoutSlug, false)
	b, tg := newFanoutBot(t, members, subs)

	if err := b.handleStopLive(context.Background(), b, cmdUpdate(99, "outsider", "/stoplive", fanoutSlug)); err != nil {
		t.Fatalf("handleStopLive: %v", err)
	}
	if !subs.hasUnsub(99, fanoutSlug) {
		t.Error("non-member issuer should be unsubscribed")
	}
	if subs.hasUnsub(10, fanoutSlug) || subs.hasUnsub(20, fanoutSlug) {
		t.Error("non-member /stoplive must not touch household members")
	}
	if tg.sendCount() != 1 {
		t.Fatalf("non-member should send only its own confirmation, got %d", tg.sendCount())
	}
}
