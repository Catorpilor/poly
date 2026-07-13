package live

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newWSPair returns both ends of a real WebSocket connection. The server
// side is what the SubscriptionRegistry holds; the client side lets tests
// read what was written.
func newWSPair(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()

	upgrader := websocket.Upgrader{}
	connCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connCh <- c
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	select {
	case server = <-connCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server-side connection")
	}
	t.Cleanup(func() { server.Close() })
	return server, client
}

// All writes to a web connection must go through the registry, which
// serializes them — gorilla/websocket forbids concurrent writers. Before
// this existed, subscribe acks (handler goroutine) raced trade broadcasts
// (RTDS goroutine) on the same conn.
func TestWriteConnSerializesConcurrentWriters(t *testing.T) {
	t.Parallel()

	server, client := newWSPair(t)
	reg := NewSubscriptionRegistry()
	reg.RegisterConn(server)

	const writers = 8
	const perWriter = 25

	// Reader drains the client side and counts frames.
	recv := make(chan int, 1)
	go func() {
		count := 0
		for count < writers*perWriter {
			if _, _, err := client.ReadMessage(); err != nil {
				break
			}
			count++
		}
		recv <- count
	}()

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := reg.WriteConn(server, []byte(`{"type":"x"}`)); err != nil {
					t.Errorf("WriteConn: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	select {
	case got := <-recv:
		if got != writers*perWriter {
			t.Errorf("client received %d frames, want %d", got, writers*perWriter)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for frames")
	}
}

// A connection must be registered before it can be written to; writes to
// unknown (or already-cleaned-up) connections report an error instead of
// racing an unsynchronized write.
func TestWriteConnRequiresRegistration(t *testing.T) {
	t.Parallel()

	server, _ := newWSPair(t)
	reg := NewSubscriptionRegistry()

	if err := reg.WriteConn(server, []byte("x")); err == nil {
		t.Fatal("WriteConn on unregistered conn = nil error, want error")
	}

	reg.RegisterConn(server)
	if err := reg.WriteConn(server, []byte("x")); err != nil {
		t.Fatalf("WriteConn after RegisterConn = %v, want nil", err)
	}

	reg.UnsubscribeWeb(server)
	if err := reg.WriteConn(server, []byte("x")); err == nil {
		t.Fatal("WriteConn after UnsubscribeWeb = nil error, want error")
	}
}

// A dead client must not stall the feed for everyone else: when a broadcast
// write fails, the connection is dropped from the registry so subsequent
// broadcasts skip it.
func TestBroadcastToWebDropsFailedConn(t *testing.T) {
	t.Parallel()

	server, client := newWSPair(t)
	m := &LiveTradeManager{
		subscriptions: NewSubscriptionRegistry(),
		formatter:     NewTradeFormatter(),
	}
	m.subscriptions.RegisterConn(server)
	m.subscriptions.SubscribeWeb(server, "test-event", false)

	// Kill the transport so the next write fails.
	server.Close()
	client.Close()

	m.broadcastToWeb("test-event", &TradeInfo{}, "", false)

	if subs := m.subscriptions.GetWebSubscribers("test-event"); len(subs) != 0 {
		t.Errorf("failed conn still subscribed after broadcast: %d subscribers", len(subs))
	}
}

// The original F6 pairing: subscribe acks (webserver handler goroutine)
// racing trade broadcasts (RTDS goroutine) on the same conn. Run under
// -race; before the shared write path, sendResponse wrote unsynchronized.
func TestSendResponseAndBroadcastShareWritePath(t *testing.T) {
	t.Parallel()

	server, client := newWSPair(t)
	m := &LiveTradeManager{
		subscriptions: NewSubscriptionRegistry(),
		formatter:     NewTradeFormatter(),
	}
	m.subscriptions.RegisterConn(server)
	m.subscriptions.SubscribeWeb(server, "test-event", false)

	webSrv := NewWebServer(m, 0, nil, nil, nil, nil)

	const rounds = 50
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < rounds; i++ {
			m.broadcastToWeb("test-event", &TradeInfo{EventSlug: "test-event"}, "", false)
		}
	}()
	for i := 0; i < rounds; i++ {
		webSrv.sendResponse(server, wsResponse{Type: "subscribed", Event: "test-event"})
	}
	<-done

	// Drain everything the client should have received: both writers ran
	// to completion without dropping the conn, so all frames arrive.
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 2*rounds; i++ {
		if _, _, err := client.ReadMessage(); err != nil {
			t.Fatalf("client read %d/%d: %v", i, 2*rounds, err)
		}
	}
}

// A healthy subscriber receives broadcasts written through the shared path.
func TestBroadcastToWebDeliversToSubscriber(t *testing.T) {
	t.Parallel()

	server, client := newWSPair(t)
	m := &LiveTradeManager{
		subscriptions: NewSubscriptionRegistry(),
		formatter:     NewTradeFormatter(),
	}
	m.subscriptions.RegisterConn(server)
	m.subscriptions.SubscribeWeb(server, "test-event", false)

	m.broadcastToWeb("test-event", &TradeInfo{EventSlug: "test-event"}, "", false)

	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if len(msg) == 0 {
		t.Error("client received empty broadcast frame")
	}
	if subs := m.subscriptions.GetWebSubscribers("test-event"); len(subs) != 1 {
		t.Errorf("healthy conn dropped after broadcast: %d subscribers, want 1", len(subs))
	}
}
