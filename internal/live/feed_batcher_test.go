package live

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fakes ---

// fakeFeedSender is a mutex-guarded TelegramSender capturing sent messages.
type fakeFeedSender struct {
	mu   sync.Mutex
	sent []sentFeedMsg
}

type sentFeedMsg struct {
	chatID int64
	text   string
}

func (s *fakeFeedSender) SendMessage(chatID int64, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sentFeedMsg{chatID: chatID, text: text})
}

func (s *fakeFeedSender) messages() []sentFeedMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentFeedMsg(nil), s.sent...)
}

// fakeFeedTimer stands in for time.AfterFunc: tests fire it manually so
// flushes are deterministic without sleeping.
type fakeFeedTimer struct {
	mu      sync.Mutex
	d       time.Duration
	fn      func()
	stopped bool
}

func (t *fakeFeedTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := t.stopped
	t.stopped = true
	return !was
}

// fire runs the callback like the real timer expiring; a stopped timer
// does not fire. The callback runs outside the timer's mutex, mirroring
// time.AfterFunc's own goroutine.
func (t *fakeFeedTimer) fire() {
	t.mu.Lock()
	fn, stopped := t.fn, t.stopped
	t.mu.Unlock()
	if !stopped {
		fn()
	}
}

// fakeFeedTimerFactory records every timer the batcher starts.
type fakeFeedTimerFactory struct {
	mu     sync.Mutex
	timers []*fakeFeedTimer
}

func (f *fakeFeedTimerFactory) newTimer(d time.Duration, fn func()) feedTimer {
	t := &fakeFeedTimer{d: d, fn: fn}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timers = append(f.timers, t)
	return t
}

func (f *fakeFeedTimerFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

func (f *fakeFeedTimerFactory) timer(i int) *fakeFeedTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timers[i]
}

// feedBatcherHarness builds a batcher with a fake sender and manual timers.
func feedBatcherHarness() (*FeedBatcher, *fakeFeedSender, *fakeFeedTimerFactory) {
	sender := &fakeFeedSender{}
	timers := &fakeFeedTimerFactory{}
	b := NewFeedBatcher(sender)
	b.newTimer = timers.newTimer
	return b, sender, timers
}

const (
	feedTestChat int64 = 7
	feedTestSlug       = "atp-norrie-shapova-2026-08-01"
)

// --- tests ---

func TestFeedBatcherFloorFiltering(t *testing.T) {
	t.Parallel()

	b, sender, timers := feedBatcherHarness()

	b.Add(feedTestChat, feedTestSlug, 19.99, "dust BUY Norrie $19.99 @ $0.50")
	if got := timers.count(); got != 0 {
		t.Fatalf("sub-floor trade started %d timers, want 0", got)
	}
	b.Flush(feedTestChat, feedTestSlug)
	if got := sender.messages(); len(got) != 0 {
		t.Fatalf("sub-floor trade was relayed: %v", got)
	}

	b.Add(feedTestChat, feedTestSlug, 20.00, "whale BUY Norrie $20.00 @ $0.50")
	if got := timers.count(); got != 1 {
		t.Fatalf("at-floor trade started %d timers, want 1", got)
	}
	timers.timer(0).fire()
	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("at-floor trade sent %d messages, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].text, "whale BUY Norrie $20.00 @ $0.50") {
		t.Errorf("flushed message missing the at-floor line: %q", msgs[0].text)
	}
}

func TestFeedBatcherSingleFlushKeepsOrder(t *testing.T) {
	t.Parallel()

	b, sender, timers := feedBatcherHarness()

	lines := []string{
		"first BUY Norrie $25.00 @ $0.40",
		"second SELL Shapovalov $30.00 @ $0.60",
		"third BUY Norrie $45.00 @ $0.41",
	}
	for _, line := range lines {
		b.Add(feedTestChat, feedTestSlug, 25, line)
	}

	if got := timers.count(); got != 1 {
		t.Fatalf("one window started %d timers, want 1 (first trade arms it, no idle ticks)", got)
	}
	if got := timers.timer(0).d; got != feedBatchWindow {
		t.Errorf("timer armed for %v, want feedBatchWindow (%v)", got, feedBatchWindow)
	}

	timers.timer(0).fire()
	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("flush sent %d messages, want 1", len(msgs))
	}
	if msgs[0].chatID != feedTestChat {
		t.Errorf("flush sent to chat %d, want %d", msgs[0].chatID, feedTestChat)
	}
	want := "[ATP-NORRIE-S] Live trades\n" + strings.Join(lines, "\n")
	if msgs[0].text != want {
		t.Errorf("flushed message = %q, want %q", msgs[0].text, want)
	}

	// The window is spent: nothing pending, a later flush sends nothing.
	b.Flush(feedTestChat, feedTestSlug)
	if got := sender.messages(); len(got) != 1 {
		t.Fatalf("post-flush Flush sent extra messages: %d total, want 1", len(got))
	}
}

func TestFeedBatcherCapsLinesWithMoreTradesTail(t *testing.T) {
	t.Parallel()

	b, sender, timers := feedBatcherHarness()

	total := feedBatchMaxLines + 3
	for i := 0; i < total; i++ {
		b.Add(feedTestChat, feedTestSlug, 50, fmt.Sprintf("trade-%d", i))
	}
	timers.timer(0).fire()

	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("flush sent %d messages, want 1", len(msgs))
	}
	got := strings.Split(msgs[0].text, "\n")
	// Header + capped lines + tail.
	wantLen := 1 + feedBatchMaxLines + 1
	if len(got) != wantLen {
		t.Fatalf("flushed message has %d lines, want %d: %q", len(got), wantLen, msgs[0].text)
	}
	for i := 0; i < feedBatchMaxLines; i++ {
		if want := fmt.Sprintf("trade-%d", i); got[1+i] != want {
			t.Errorf("line %d = %q, want %q", i, got[1+i], want)
		}
	}
	if want := "+3 more trades"; got[len(got)-1] != want {
		t.Errorf("tail = %q, want %q", got[len(got)-1], want)
	}
}

func TestFeedBatcherSeparateChatsAndSubscriptions(t *testing.T) {
	t.Parallel()

	b, sender, timers := feedBatcherHarness()

	otherChat := feedTestChat + 1
	otherSlug := "nba-lal-por-2026-08-01"

	b.Add(feedTestChat, feedTestSlug, 25, "tennis for chat 7")
	b.Add(feedTestChat, otherSlug, 25, "nba for chat 7")
	b.Add(otherChat, feedTestSlug, 25, "tennis for chat 8")

	if got := timers.count(); got != 3 {
		t.Fatalf("3 (chat, subscription) pairs started %d timers, want 3", got)
	}
	for i := 0; i < 3; i++ {
		timers.timer(i).fire()
	}

	msgs := sender.messages()
	if len(msgs) != 3 {
		t.Fatalf("flushes sent %d messages, want 3", len(msgs))
	}
	for _, msg := range msgs {
		if lines := strings.Split(msg.text, "\n"); len(lines) != 2 {
			t.Errorf("buffers shared lines: chat %d got %q", msg.chatID, msg.text)
		}
	}
}

func TestFeedBatcherFlushOnUnsubscribe(t *testing.T) {
	t.Parallel()

	b, sender, timers := feedBatcherHarness()

	b.Add(feedTestChat, feedTestSlug, 25, "pending BUY Norrie $25.00 @ $0.40")
	b.Flush(feedTestChat, feedTestSlug)

	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("unsubscribe flush sent %d messages, want 1 immediately", len(msgs))
	}
	if !strings.Contains(msgs[0].text, "pending BUY Norrie") {
		t.Errorf("unsubscribe flush missing pending line: %q", msgs[0].text)
	}

	// The pending timer was stopped; expiring it must not double-send.
	timers.timer(0).fire()
	if got := sender.messages(); len(got) != 1 {
		t.Fatalf("stopped timer still flushed: %d messages, want 1", len(got))
	}
}

func TestFeedBatcherEmptyBufferSendsNothing(t *testing.T) {
	t.Parallel()

	b, sender, _ := feedBatcherHarness()

	b.Flush(feedTestChat, feedTestSlug)
	b.FlushAll()
	if got := sender.messages(); len(got) != 0 {
		t.Fatalf("empty batcher sent %d messages, want 0", len(got))
	}
}

func TestFeedBatcherNewWindowAfterFlush(t *testing.T) {
	t.Parallel()

	b, sender, timers := feedBatcherHarness()

	b.Add(feedTestChat, feedTestSlug, 25, "window-1")
	timers.timer(0).fire()
	b.Add(feedTestChat, feedTestSlug, 25, "window-2")
	if got := timers.count(); got != 2 {
		t.Fatalf("second window started %d timers total, want 2", got)
	}
	timers.timer(1).fire()

	msgs := sender.messages()
	if len(msgs) != 2 {
		t.Fatalf("two windows sent %d messages, want 2", len(msgs))
	}
	if strings.Contains(msgs[1].text, "window-1") {
		t.Errorf("second window resent first window's line: %q", msgs[1].text)
	}
}

func TestFeedBatcherConcurrentAddsDuringFlush(t *testing.T) {
	t.Parallel()

	b, sender, _ := feedBatcherHarness()

	const adders = 4
	const addsPerAdder = 200

	var wg sync.WaitGroup
	for a := 0; a < adders; a++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < addsPerAdder; i++ {
				b.Add(feedTestChat, feedTestSlug, 25, "L")
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < addsPerAdder; i++ {
			b.Flush(feedTestChat, feedTestSlug)
		}
	}()

	wg.Wait()
	<-done
	b.FlushAll()

	// Every surviving add is accounted for exactly once: as a line or in a
	// "+N more trades" tail.
	got := 0
	for _, msg := range sender.messages() {
		for _, line := range strings.Split(msg.text, "\n") {
			if line == "L" {
				got++
				continue
			}
			var n int
			if _, err := fmt.Sscanf(line, "+%d more trades", &n); err == nil {
				got += n
			}
		}
	}
	if want := adders * addsPerAdder; got != want {
		t.Fatalf("concurrent adds accounted %d trades across flushes, want %d", got, want)
	}
}
