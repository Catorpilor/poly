package live

import (
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
)

// Issue #92: a zero-balance sell rejection on a freshly created arm is far
// more likely a fill still settling than a position closed outside the bot.
// On the SL path the monitor skips the fire inside the settle grace and the
// next evaluation retries (single-flight already released). The TP paths get
// NO grace (review F4): ClearTP / ceiling Disarm / AdvanceLadder commit
// before the sell, so a skip cannot retry — it would strand the arm silently
// unprotected. There the old disarm-and-notify behavior stands, with a
// distinct log line for September counting.

func settleArm(created time.Time) *database.SLTPArm {
	return &database.SLTPArm{
		ID: 501, TelegramID: 91, TokenID: "SETTLE1",
		AvgPrice: 0.07, SharesAtArm: 71.43, HighWaterMark: 0.07,
		TPArmed: true, CreatedAt: created,
	}
}

// The TP path disarms even inside the settle window — ClearTP already
// committed, so a graced skip would leave tp_armed=false with no retry and no
// notice (strictly worse than the notify-and-disarm it replaces).
func TestTPShortfall_SettlingArm_StillDisarmsAndNotifies(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	feed := newFakeFeed()
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, &fakeExecutor{}, notif)

	arm := settleArm(clock.now().Add(-10 * time.Second))
	store.seed(arm)
	_, handled := m.retryTPShortfall("TP", arm, 17_857_142, 0.17, shortfallResult(0))

	if !handled {
		t.Fatalf("want handled=true")
	}
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 1 {
		t.Errorf("disarm calls = %d, want 1 (TP path has no settle grace — it cannot retry)", disarms)
	}
	notif.mu.Lock()
	stales := len(notif.stales)
	notif.mu.Unlock()
	if stales != 1 {
		t.Errorf("stale notices = %d, want 1 (the user must hear about the disarm)", stales)
	}
}

func TestTPShortfall_OldArm_StillDisarms(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	feed := newFakeFeed()
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, &fakeExecutor{}, notif)

	arm := settleArm(clock.now().Add(-10 * time.Minute))
	store.seed(arm)
	_, handled := m.retryTPShortfall("TP", arm, 17_857_142, 0.17, shortfallResult(0))

	if !handled {
		t.Fatalf("gone-position shortfall must be handled, got handled=false")
	}
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 1 {
		t.Errorf("disarm calls = %d, want 1 (position genuinely gone)", disarms)
	}
}

// Legacy rows without a scanned CreatedAt (zero time) must keep the old
// behavior — the grace never blocks a real gone-position disarm forever.
func TestTPShortfall_ZeroCreatedAt_StillDisarms(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	feed := newFakeFeed()
	notif := &fakeNotifier{}
	m, _ := slBreachMonitor(store, feed, &fakeExecutor{}, notif)

	arm := settleArm(time.Time{})
	store.seed(arm)
	_, handled := m.retryTPShortfall("TP", arm, 17_857_142, 0.17, shortfallResult(0))

	if !handled {
		t.Fatalf("want handled=true")
	}
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 1 {
		t.Errorf("disarm calls = %d, want 1 for a zero-CreatedAt arm", disarms)
	}
}

func TestSLShortfall_SettlingArm_NoDisarm(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	feed := newFakeFeed()
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, &fakeExecutor{}, notif)

	arm := settleArm(clock.now().Add(-10 * time.Second))
	arm.SLArmed = true
	m.handleSLShortfall(arm, 0, 0.05)

	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 0 {
		t.Errorf("disarmed a settling arm on the SL path: %d disarm calls, want 0", disarms)
	}
	notif.mu.Lock()
	stales := len(notif.stales)
	notif.mu.Unlock()
	if stales != 0 {
		t.Errorf("sent %d stale notices for a settling arm, want 0", stales)
	}
}

func TestSLShortfall_OldArm_StillDisarms(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	feed := newFakeFeed()
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, &fakeExecutor{}, notif)

	arm := settleArm(clock.now().Add(-10 * time.Minute))
	arm.SLArmed = true
	store.seed(arm)
	m.handleSLShortfall(arm, 0, 0.05)

	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 1 {
		t.Errorf("disarm calls = %d, want 1 (position genuinely gone)", disarms)
	}
}
