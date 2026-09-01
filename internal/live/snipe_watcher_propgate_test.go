package live

import (
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// startedPropMarket is startedMarket with a period/total prop question, i.e. a
// token the watcher must gate to a log-only prop-gated line (September review
// proposal #3, "corpse-by-clock").
func startedPropMarket(tokenID, question string) SnipeMarket {
	m := startedMarket(tokenID)
	m.Question = question
	return m
}

// TestSnipeWatcher_PropGated pins the September review proposal #3 gate: a
// period/total prop (O/U class) never dispatches — no in-band alert, no deep DM,
// no boxed rung — and instead emits exactly one house-style "SnipeWatcher:
// prop-gated" line per episode, with identical once-per-episode and reset
// semantics to a real alert. Not parallel: it redirects the global logger.
func TestSnipeWatcher_PropGated(t *testing.T) {
	const celta = "RC Celta de Vigo vs. CA Osasuna: 1st Half O/U 1.5"

	t.Run("Celta episode: zero dispatches, exactly one prop-gated line, correct fields", func(t *testing.T) {
		buf := &syncBuffer{}
		log.SetOutput(buf)
		t.Cleanup(func() { log.SetOutput(os.Stderr) })

		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedPropMarket("T1", celta)})
		w.evaluate("T1", 0.69, 0.70) // ratchet high to 0.69
		w.evaluate("T1", 0.10, 0.19) // in-band crash → prop-gated, NOT dispatched

		if notif.count() != 0 || notif.deepCount() != 0 || notif.boxedCount() != 0 {
			t.Fatalf("dispatches alerts=%d deeps=%d boxed=%d, want 0/0/0", notif.count(), notif.deepCount(), notif.boxedCount())
		}
		out := buf.String()
		if n := strings.Count(out, "SnipeWatcher: prop-gated"); n != 1 {
			t.Fatalf("prop-gated lines = %d, want 1; log:\n%s", n, out)
		}
		if strings.Contains(out, "SnipeWatcher: alert ") {
			t.Errorf("emitted an alert line for a prop; log:\n%s", out)
		}
		if strings.Contains(out, "SnipeWatcher: favorite-collapse") {
			t.Errorf("emitted a favorite-collapse line for a prop; log:\n%s", out)
		}
		for _, want := range []string{"token=T1", "high=0.690", "ask=0.190", `q="` + celta + `"`} {
			if !strings.Contains(out, want) {
				t.Errorf("prop-gated log missing %q in:\n%s", want, out)
			}
		}
	})

	t.Run("continued collapse into the deep zone: no deep DM, no extra prop-gated line", func(t *testing.T) {
		buf := &syncBuffer{}
		log.SetOutput(buf)
		t.Cleanup(func() { log.SetOutput(os.Stderr) })

		w, _, rec, notif, clock := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedPropMarket("T1", celta)})
		w.evaluate("T1", 0.69, 0.70)
		w.evaluate("T1", 0.10, 0.19) // prop-gated #1
		clock.advance(3 * time.Minute)
		w.evaluate("T1", 0.01, 0.02) // deep zone — must stay silent for a prop

		if notif.deepCount() != 0 {
			t.Fatalf("deep DM fired for a prop token")
		}
		if n := strings.Count(buf.String(), "SnipeWatcher: prop-gated"); n != 1 {
			t.Errorf("prop-gated lines = %d, want 1 (deep collapse adds none)", n)
		}
	})

	t.Run("boxed shape: prop passes its in-band fire then the boxed zone — zero rungs", func(t *testing.T) {
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedPropMarket("T1", celta)})
		w.evaluate("T1", 0.69, 0.70) // ratchet high
		w.evaluate("T1", 0.10, 0.17) // in-band: prop-gated (no dispatch); a non-prop would alert here
		// The boxed ladder rides the alerted latch a prop DOES set, so without an
		// explicit exclusion it would BUY rungs on the prop flip side. It must not.
		w.evaluate("T1", 0.02, 0.04) // boxed zone (both rungs for a non-prop)
		w.evaluate("T1", 0.02, 0.04)
		if notif.boxedCount() != 0 || notif.count() != 0 || notif.deepCount() != 0 {
			t.Errorf("prop boxed=%d alerts=%d deeps=%d, want 0/0/0", notif.boxedCount(), notif.count(), notif.deepCount())
		}
	})

	t.Run("episode semantics: same episode one line; genuine reset then re-crash a second", func(t *testing.T) {
		buf := &syncBuffer{}
		log.SetOutput(buf)
		t.Cleanup(func() { log.SetOutput(os.Stderr) })

		w, _, rec, _, clock := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedPropMarket("T1", celta)})
		w.evaluate("T1", 0.69, 0.70)
		w.evaluate("T1", 0.10, 0.19) // prop-gated #1
		w.evaluate("T1", 0.10, 0.18) // same episode, still latched → no new line
		if n := strings.Count(buf.String(), "SnipeWatcher: prop-gated"); n != 1 {
			t.Fatalf("after a same-episode re-tick prop-gated lines = %d, want 1", n)
		}
		w.evaluate("T1", 0.32, 0.34) // corroborated recovery: streak starts
		clock.advance(snipeResetConfirm + time.Second)
		w.evaluate("T1", 0.32, 0.34) // held past confirm: episode resets
		w.evaluate("T1", 0.08, 0.16) // re-crash → prop-gated #2 (per-episode)
		if n := strings.Count(buf.String(), "SnipeWatcher: prop-gated"); n != 2 {
			t.Errorf("after reset+re-crash prop-gated lines = %d, want 2", n)
		}
	})

	t.Run("high >= 0.90 prop: prop-gated line only, no favorite-collapse line", func(t *testing.T) {
		buf := &syncBuffer{}
		log.SetOutput(buf)
		t.Cleanup(func() { log.SetOutput(os.Stderr) })

		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedPropMarket("T1", celta)})
		w.evaluate("T1", 0.92, 0.94) // favorite-tier high on a prop
		w.evaluate("T1", 0.10, 0.17) // crash

		out := buf.String()
		if notif.count() != 0 {
			t.Fatalf("prop dispatched an alert at high>=0.90")
		}
		if strings.Contains(out, "SnipeWatcher: favorite-collapse") {
			t.Errorf("prop emitted a favorite-collapse line; log:\n%s", out)
		}
		if n := strings.Count(out, "SnipeWatcher: prop-gated"); n != 1 {
			t.Errorf("prop-gated lines = %d, want 1; log:\n%s", n, out)
		}
	})
}

// TestSnipeWatcher_NonPropUnaffected is the regression guard: spread props and
// game-winner markets travel the identical price path a prop does but must
// dispatch exactly as today — the gate touches only the O/U class.
func TestSnipeWatcher_NonPropUnaffected(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"Spread: FC Barcelona (-2.5)",
		"LoL: Nongshim Red Force vs BNK FEARX - Game 2 Winner",
	} {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			w, _, rec, notif, _ := snipeHarness()
			rec.eventSubs["evt"] = []int64{101}
			w.WatchEventMarkets("evt", []SnipeMarket{startedPropMarket("T1", q)})
			w.evaluate("T1", 0.69, 0.70)
			w.evaluate("T1", 0.10, 0.19) // in-band crash → real alert, unchanged
			if notif.count() != 1 {
				t.Fatalf("alerts=%d for %q, want 1 (non-prop must dispatch as today)", notif.count(), q)
			}
			if r := notif.recipients(); len(r) != 1 || r[0] != 101 {
				t.Errorf("recipients=%v, want [101]", r)
			}
		})
	}
}
