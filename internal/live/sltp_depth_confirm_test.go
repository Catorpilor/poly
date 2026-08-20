package live

import (
	"bytes"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// depthCooldownActive reports whether arm.ID's depth-confirm cooldown is armed
// (white-box: a refusal has stamped refusedAt). Used as the settled signal
// after an async refusal before a suppression assertion.
func depthCooldownActive(m *SLTPMonitor, armID int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.depthConfirm[armID]
	return st != nil && !st.refusedAt.IsZero()
}

// TestSLTPMonitor_ConfirmFire_Decision pins the depth-aware fire decision
// (issue #80) across all three kinds — including the four live regression
// exhibits, the strict-comparison boundaries, the fail-open path, and the
// partial-depth (depth < fired size) asymmetry between TP and SL — sourced from
// the fresh-book reader. The fired size is a fixed 100 shares; `depth` is
// scripted relative to it.
func TestSLTPMonitor_ConfirmFire_Decision(t *testing.T) {
	t.Parallel()
	const size = 100.0
	cases := []struct {
		name      string
		kind      string
		threshold float64 // arm's TP trigger / SL stop / ceiling price
		fireBid   float64 // the print that triggered the candidate fire
		vwap      float64
		depth     float64
		ok        bool
		wantFire  bool
	}{
		// --- live exhibits: phantom prints must be refused ---
		// r68 TP (08-16): fired at 0.52, real sell VWAP ~0.21, 2× trigger 0.42.
		{"r68 TP phantom", "TP", 0.42, 0.52, 0.21, 1000, true, false},
		// v0.20.0 ladder rung-1: fired at 0.24, real book VWAP 0.0903, trigger 0.20.
		{"ladder rung TP phantom", "TP", 0.20, 0.24, 0.0903, 1000, true, false},
		// HB-series ceiling (08-05): thin 0.95 print, real VWAP 0.7813 for 218 sh.
		{"HB ceiling phantom", "ceiling", 0.95, 0.95, 0.7813, 1000, true, false},
		// July SL root-cause #3 (r17): phantom low 0.06, real VWAP 0.153 above the
		// 0.10 stop — the stop must NOT fire.
		{"SL phantom low", "SL", 0.10, 0.06, 0.153, 1000, true, false},

		// --- genuine fires: VWAP confirms the print ---
		{"TP genuine", "TP", 0.20, 0.21, 0.21, 1000, true, true},
		{"ceiling genuine", "ceiling", 0.95, 0.96, 0.951, 1000, true, true},
		// Real collapse: VWAP below the stop → the stop fires.
		{"SL genuine collapse", "SL", 0.10, 0.06, 0.05, 1000, true, true},

		// --- strict comparison, no tolerance ---
		// TP fires iff VWAP >= threshold: exactly-at fires.
		{"TP exactly at threshold fires", "TP", 0.20, 0.20, 0.20, 1000, true, true},
		// SL fires iff VWAP < stop: exactly-at does NOT fire (book at the stop).
		{"SL exactly at stop refused", "SL", 0.10, 0.06, 0.10, 1000, true, false},

		// --- fail-open: no usable fresh book (ok=false) always fires ---
		{"TP fail-open", "TP", 0.20, 0.52, 0, 0, false, true},
		{"SL fail-open", "SL", 0.10, 0.06, 0, 0, false, true},

		// --- partial depth (depth < size) ---
		// TP: partial VWAP is an UPPER bound; below threshold still proves a miss.
		{"TP partial depth refused", "TP", 0.42, 0.52, 0.21, 50, true, false},
		// SL: a partial fill can't prove the book clears the stop → fails open
		// (the stop fires), even though the partial VWAP sits above the stop.
		{"SL partial depth fires", "SL", 0.10, 0.06, 0.20, 50, true, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := newFakeBookReader()
			reader.setVWAP("TOK", tc.vwap, tc.depth, tc.ok)
			m := NewSLTPMonitor(newFakeStore(), newFakeFeed(), &fakeExecutor{}, &fakeNotifier{}, nil)
			m.SetBookReader(reader)
			arm := &database.SLTPArm{ID: 1, TelegramID: 9, TokenID: "TOK"}
			if got := m.confirmFire(arm, tc.kind, tc.fireBid, tc.threshold, size); got != tc.wantFire {
				t.Errorf("confirmFire(%s) = %v, want %v", tc.kind, got, tc.wantFire)
			}
		})
	}
}

// TestSLTPMonitor_ConfirmFire_NilReaderFailsOpen proves a monitor without a
// book reader wired fires unconditionally (pre-#80 one-bid behavior) — the
// guard degrades to a no-op.
func TestSLTPMonitor_ConfirmFire_NilReaderFailsOpen(t *testing.T) {
	t.Parallel()
	m := NewSLTPMonitor(newFakeStore(), newFakeFeed(), &fakeExecutor{}, &fakeNotifier{}, nil)
	arm := &database.SLTPArm{ID: 1, TelegramID: 9, TokenID: "TOK"}
	if !m.confirmFire(arm, "TP", 0.52, 0.42, 100) {
		t.Fatal("nil book reader must fail open (fire)")
	}
}

// TestSLTPMonitor_ConfirmFire_RefusedLog checks the exact depth-refused line
// format the production monitor greps for.
func TestSLTPMonitor_ConfirmFire_RefusedLog(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	reader := newFakeBookReader()
	reader.setVWAP("LG", 0.21, 1000, true)
	m := NewSLTPMonitor(newFakeStore(), newFakeFeed(), &fakeExecutor{}, &fakeNotifier{}, nil)
	m.SetBookReader(reader)
	arm := &database.SLTPArm{ID: 4, TelegramID: 42, TokenID: "LG"}
	if m.confirmFire(arm, "TP", 0.52, 0.42, 25) {
		t.Fatal("expected refusal")
	}
	want := "SLTPMonitor: depth-refused kind=TP user=42 token=LG fireBid=0.5200 execVWAP=0.2100 size=25.00"
	if got := buf.String(); !strings.Contains(got, want) {
		t.Errorf("log = %q\nwant substring %q", got, want)
	}
}

// --- integration: the wiring gates ExecuteSell, re-arms, and bounds I/O ---

// TestSLTPMonitor_TP_RefuseReArmCooldown exercises the full loop: a phantom TP
// is refused (no sell, TP stays armed), a retry inside the cooldown is
// suppressed BEFORE the holdings read (F2) and the book fetch (no new calls),
// and after the cooldown a genuinely-crossed book fires.
func TestSLTPMonitor_TP_RefuseReArmCooldown(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// TP-only auto-arm (SLArmed=false) so fireTP reads holdings for its basis.
	store.seed(&database.SLTPArm{ID: 20, TelegramID: 8, TokenID: "PT", AvgPrice: 0.10, SharesAtArm: 100,
		HighWaterMark: 0.10, TickSize: 0.01, TPArmed: true, SLArmed: false})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	reader := newFakeBookReader()
	holdings := &fakeHoldings{raw: map[string]int64{"PT": 100_000_000}}
	clock := newFakeClock()
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.now = clock.now
	m.SetHoldingReader(holdings)
	m.SetBookReader(reader)
	_ = m.Start()

	// Phantom: bid 0.24 crosses the 0.20 trigger, book VWAP 0.09 far below it.
	reader.setVWAP("PT", 0.09, 1000, true)
	feed.setBid("PT", 0.24)
	feed.emit("PT")
	waitFor(t, func() bool { return depthCooldownActive(m, 20) })

	if exec.callCountTotal() != 0 {
		t.Fatalf("phantom TP must not sell, got %d calls", exec.callCountTotal())
	}
	if store.armedCount("PT") != 1 || !store.byToken["PT"][0].TPArmed {
		t.Fatal("TP must stay armed after a refusal (re-arm, not consume)")
	}
	if reader.callCount() != 1 || holdings.callCount() != 1 {
		t.Fatalf("first attempt: reader=%d holdings=%d, want 1/1", reader.callCount(), holdings.callCount())
	}

	// Retry INSIDE the cooldown: the claim gate bails before holdings/fetch.
	feed.emit("PT")
	time.Sleep(50 * time.Millisecond)
	if reader.callCount() != 1 {
		t.Errorf("cooldown must suppress the book fetch; reader calls = %d, want 1", reader.callCount())
	}
	if holdings.callCount() != 1 {
		t.Errorf("cooldown must suppress the holdings read (F2); holdings calls = %d, want 1", holdings.callCount())
	}
	if exec.callCountTotal() != 0 {
		t.Fatalf("still no sell during cooldown, got %d", exec.callCountTotal())
	}

	// After the cooldown a genuinely-crossed book fires.
	clock.advance(5 * time.Second)
	reader.setVWAP("PT", 0.21, 1000, true)
	feed.emit("PT")
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})
	exec.mu.Lock()
	got := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if want := int64(100 * database.TPSellFraction * 1e6); got != want {
		t.Errorf("TP sell size = %d, want %d", got, want)
	}
	if reader.callCount() != 2 || holdings.callCount() != 2 {
		t.Errorf("post-cooldown attempt: reader=%d holdings=%d, want 2/2", reader.callCount(), holdings.callCount())
	}
}

// TestSLTPMonitor_ConfirmFire_ConcurrentSingleFlight (F3) fires many concurrent
// evals at a phantom book with a frozen clock and asserts exactly ONE fresh-book
// fetch and no sell — the atomic claim gate serializes the confirm so concurrent
// WS+tick evals can't double-fetch or double-log. Meaningful under -race.
func TestSLTPMonitor_ConfirmFire_ConcurrentSingleFlight(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 21, TelegramID: 8, TokenID: "CC", AvgPrice: 0.10, SharesAtArm: 100,
		HighWaterMark: 0.10, TickSize: 0.01, TPArmed: true, SLArmed: false})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	reader := newFakeBookReader()
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.now = newFakeClock().now // frozen: any refusal's cooldown blocks the rest
	m.SetBookReader(reader)
	_ = m.Start()

	reader.setVWAP("CC", 0.09, 1000, true) // phantom below the 0.20 trigger
	feed.setBid("CC", 0.24)

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); feed.emit("CC") }()
	}
	wg.Wait()
	waitFor(t, func() bool { return reader.callCount() >= 1 })
	time.Sleep(50 * time.Millisecond) // allow any stragglers to (not) fetch

	if reader.callCount() != 1 {
		t.Errorf("concurrent evals must yield ONE fresh-book fetch, got %d", reader.callCount())
	}
	if exec.callCountTotal() != 0 {
		t.Errorf("phantom must not sell, got %d", exec.callCountTotal())
	}
}

func TestSLTPMonitor_Ceiling_PhantomRefused_GenuineFires_FailOpen(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		vwap     float64
		ok       bool
		wantFire bool
	}{
		{"phantom thin 0.95 refused", 0.78, true, false},
		{"deep book fires", 0.951, true, true},
		{"no book fails open", 0, false, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeStore()
			store.seed(&database.SLTPArm{ID: 30, TelegramID: 6, TokenID: "CT", AvgPrice: 0.50, SharesAtArm: 200,
				HighWaterMark: 0.95, TPArmed: true, SLArmed: true})
			feed := newFakeFeed()
			exec := &fakeExecutor{}
			notif := &fakeNotifier{}
			reader := newFakeBookReader()
			m := NewSLTPMonitor(store, feed, exec, notif, nil)
			m.SetBookReader(reader)
			_ = m.Start()

			reader.setVWAP("CT", tc.vwap, 100000, tc.ok)
			feed.setBid("CT", 0.95) // at the ceiling
			feed.emit("CT")

			if tc.wantFire {
				waitFor(t, func() bool {
					notif.mu.Lock()
					defer notif.mu.Unlock()
					return len(notif.fires) == 1 && notif.fires[0].kind == "TP-ceiling"
				})
			} else {
				waitFor(t, func() bool { return reader.callCount() >= 1 })
				time.Sleep(30 * time.Millisecond)
				if exec.callCountTotal() != 0 {
					t.Fatalf("phantom ceiling must not sell, got %d calls", exec.callCountTotal())
				}
				if store.armedCount("CT") != 1 {
					t.Fatal("ceiling arm must stay armed after a refusal")
				}
			}
		})
	}
}

func TestSLTPMonitor_SL_PhantomLowRefused(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Active stop: avg=0.10, hwm=0.12 → trigger = max(0.10, 0.096) = 0.10.
	store.seed(&database.SLTPArm{ID: 40, TelegramID: 5, TokenID: "SX", AvgPrice: 0.10, SharesAtArm: 100,
		HighWaterMark: 0.12, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	reader := newFakeBookReader()
	m, clock := slBreachMonitor(store, feed, exec, notif)
	m.SetBookReader(reader)
	_ = m.Start()

	// Phantom low 0.06 breaches the 0.10 stop, but the real book VWAP 0.153 is
	// above it — the collapse is not real, so the stop must not fire.
	reader.setVWAP("SX", 0.153, 1000, true)
	feed.setBid("SX", 0.06)
	feed.emit("SX")
	waitFor(t, func() bool { return breachStamped(m, 40) })
	clock.advance(31 * time.Second)
	feed.emit("SX")
	waitFor(t, func() bool { return reader.callCount() >= 1 })

	if exec.callCountTotal() != 0 {
		t.Fatalf("phantom-low SL must not sell, got %d calls", exec.callCountTotal())
	}
	if store.armedCount("SX") != 1 {
		t.Fatal("SL must stay armed after a depth refusal")
	}
}

func TestSLTPMonitor_SL_GenuineCollapseFires(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 41, TelegramID: 5, TokenID: "SY", AvgPrice: 0.10, SharesAtArm: 100,
		HighWaterMark: 0.12, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	reader := newFakeBookReader()
	m, clock := slBreachMonitor(store, feed, exec, notif)
	m.SetBookReader(reader)
	_ = m.Start()

	// Real collapse: book VWAP 0.05 is below the 0.10 stop → fire.
	reader.setVWAP("SY", 0.05, 1000, true)
	feed.setBid("SY", 0.06)
	feed.emit("SY")
	waitFor(t, func() bool { return breachStamped(m, 41) })
	clock.advance(31 * time.Second)
	feed.emit("SY")
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})
	exec.mu.Lock()
	ot := exec.calls[0].orderType
	exec.mu.Unlock()
	if ot != polymarket.OrderTypeFOK {
		t.Errorf("expected a floored FOK SL exit, got %v", ot)
	}
}
