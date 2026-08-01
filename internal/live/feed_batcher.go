package live

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Telegram live-feed batching policy (issue #31). Product policy, not
// per-user config: the feed emits at most one message per (chat,
// subscription) per window, so a trade flood can never crowd SL/TP fires or
// snipe alerts — which keep their direct send paths — out of Telegram's
// per-chat rate budget.
const (
	// feedBatchWindow is how long the first buffered trade waits before its
	// batch flushes as one message.
	feedBatchWindow = 5 * time.Second
	// FeedMinTradeUSD is the relay floor: trades below this USD value are
	// dropped from the Telegram feed. The web panel feed is unfiltered.
	FeedMinTradeUSD = 20.0
	// feedBatchMaxLines caps the trade lines in one flushed message;
	// overflow is summarized as "+N more trades".
	feedBatchMaxLines = 10
)

// feedTimer is the stoppable handle behind a batch's pending flush —
// time.AfterFunc in production, a manually fired fake in tests.
type feedTimer interface {
	Stop() bool
}

// feedBatchKey identifies one chat's buffer for one subscription slug.
type feedBatchKey struct {
	chatID    int64
	eventSlug string
}

// feedBatch accumulates one window's trade lines for a key.
type feedBatch struct {
	lines []string // first feedBatchMaxLines lines in arrival order, newest last
	extra int      // trades beyond the cap, reported as "+N more trades"
	timer feedTimer
}

// FeedBatcher coalesces the Telegram live trade feed into one message per
// (chat, subscription) per feedBatchWindow and drops sub-floor trades. The
// first buffered trade arms the window's flush timer; an empty batcher is
// idle — no ticks.
type FeedBatcher struct {
	send func(chatID int64, text string)
	// newTimer schedules a flush; tests replace it to fire deterministically.
	newTimer func(d time.Duration, fn func()) feedTimer

	mu      sync.Mutex
	batches map[feedBatchKey]*feedBatch
}

// NewFeedBatcher returns a batcher delivering flushed batches through sender.
func NewFeedBatcher(sender TelegramSender) *FeedBatcher {
	return &FeedBatcher{
		send: sender.SendMessage,
		newTimer: func(d time.Duration, fn func()) feedTimer {
			return time.AfterFunc(d, fn)
		},
		batches: make(map[feedBatchKey]*feedBatch),
	}
}

// Add buffers one formatted trade line for (chatID, eventSlug), arming the
// window's flush timer on the first line. Trades under FeedMinTradeUSD are
// dropped.
func (b *FeedBatcher) Add(chatID int64, eventSlug string, tradeUSD float64, line string) {
	if tradeUSD < FeedMinTradeUSD {
		return
	}

	key := feedBatchKey{chatID: chatID, eventSlug: eventSlug}
	b.mu.Lock()
	defer b.mu.Unlock()

	batch, ok := b.batches[key]
	if !ok {
		batch = &feedBatch{}
		b.batches[key] = batch
		batch.timer = b.newTimer(feedBatchWindow, func() { b.flush(key) })
	}
	if len(batch.lines) < feedBatchMaxLines {
		batch.lines = append(batch.lines, line)
	} else {
		batch.extra++
	}
}

// Flush delivers (chatID, eventSlug)'s pending batch immediately and stops
// its timer. Called on unsubscribe so a torn-down subscription never leaves
// a pending window behind. A no-op when nothing is buffered.
func (b *FeedBatcher) Flush(chatID int64, eventSlug string) {
	b.flush(feedBatchKey{chatID: chatID, eventSlug: eventSlug})
}

// FlushAll delivers every pending batch — the best-effort shutdown drain.
func (b *FeedBatcher) FlushAll() {
	b.mu.Lock()
	keys := make([]feedBatchKey, 0, len(b.batches))
	for key := range b.batches {
		keys = append(keys, key)
	}
	b.mu.Unlock()

	for _, key := range keys {
		b.flush(key)
	}
}

// flush removes key's batch and sends it as one message. Sending happens
// outside the mutex so concurrent Adds open a fresh window instead of
// blocking behind Telegram I/O. Stopping an already-expired timer is a
// harmless no-op, so the timer-fired and unsubscribe paths share this.
func (b *FeedBatcher) flush(key feedBatchKey) {
	b.mu.Lock()
	batch, ok := b.batches[key]
	if ok {
		delete(b.batches, key)
		batch.timer.Stop()
	}
	b.mu.Unlock()

	if !ok || len(batch.lines) == 0 {
		return
	}
	b.send(key.chatID, formatFeedBatch(key.eventSlug, batch))
}

// formatFeedBatch renders one flushed batch: the subscription context
// header, the buffered lines newest last, and the overflow tail.
func formatFeedBatch(eventSlug string, batch *feedBatch) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s] Live trades", ShortenEventSlug(eventSlug))
	for _, line := range batch.lines {
		sb.WriteByte('\n')
		sb.WriteString(line)
	}
	if batch.extra > 0 {
		fmt.Fprintf(&sb, "\n+%d more trades", batch.extra)
	}
	return sb.String()
}
