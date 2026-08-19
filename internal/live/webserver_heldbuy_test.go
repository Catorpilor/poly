package live

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Catorpilor/poly/internal/config"
	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
	"github.com/Catorpilor/poly/internal/wallet"
)

// fakeTradeExecutor is a scripted webTradeExecutor: it returns a fixed
// (result, err) and records the number of Execute calls.
type fakeTradeExecutor struct {
	result *polymarket.TradeResult
	err    error
	calls  int
}

func (f *fakeTradeExecutor) Execute(context.Context, *ecdsa.PrivateKey, common.Address, *polymarket.TradeRequest) (*polymarket.TradeResult, error) {
	f.calls++
	return f.result, f.err
}

// newHeldBuyWebServer wires a WebServer over a snipe-wired manager with a real
// AES wallet manager (so DecryptPrivateKey succeeds) and a scripted executor.
// It returns the server, the watcher (for held-registration assertions), and
// the request body targeting the Moneyline BLG token (ml-blg).
func newHeldBuyWebServer(t *testing.T, exec *fakeTradeExecutor) (*WebServer, *SnipeWatcher, string) {
	t.Helper()

	m, w, _ := newSnipeWiredManager(t)

	wm, err := wallet.NewManager(strings.Repeat("a", 64)) // 32 bytes hex-encoded
	if err != nil {
		t.Fatalf("wallet.NewManager: %v", err)
	}
	kw, err := wm.GenerateNewWallet()
	if err != nil {
		t.Fatalf("GenerateNewWallet: %v", err)
	}
	enc, err := wm.EncryptPrivateKey(kw)
	if err != nil {
		t.Fatalf("EncryptPrivateKey: %v", err)
	}

	cfg := &config.Config{}
	cfg.App.LiveWebURL = "http://localhost:8081"
	trading := polymarket.NewTradingClient("http://clob.test", 137)
	ws := NewWebServer(m, 0, nil, cfg, wm, trading)
	ws.tradeExecutor = exec
	ws.userRepo = &fakeSubUserRepo{user: &database.User{
		TelegramID:   42,
		ProxyAddress: "0xproxy",
		EncryptedKey: enc,
	}}

	body := fmt.Sprintf(
		`{"session":{"telegramId":42,"walletAddress":"0xeoa","proxyAddress":"0xproxy"},`+
			`"trade":{"eventSlug":%q,"marketIndex":0,"outcomeIndex":0,"side":"BUY","amount":10}}`,
		pinnedFeedEventSlug)
	return ws, w, body
}

func postTrade(t *testing.T, ws *WebServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://localhost:8081/api/trade", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ws.handleTrade(rec, req)
	return rec
}

// waitWatched polls until tokenID is watched or the timeout elapses — the
// held registration runs in its own goroutine off the response path.
func waitWatched(w *SnipeWatcher, tokenID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if w.isWatched(tokenID) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return w.isWatched(tokenID)
}

func TestHandleTradeRegistersHeldOnSuccess(t *testing.T) {
	t.Parallel()
	exec := &fakeTradeExecutor{result: &polymarket.TradeResult{Success: true, OrderID: "ord-1"}}
	ws, w, body := newHeldBuyWebServer(t, exec)

	rec := postTrade(t, ws, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !waitWatched(w, "ml-blg", time.Second) {
		t.Fatal("ml-blg not registered as held after a successful web buy")
	}
	if !w.RenewHeldMarket(42, "ml-blg", SnipeHeldTTL) {
		t.Error("buyer chatID 42 is not a holder of ml-blg")
	}
}

func TestHandleTradeNoRegistrationOnExecutorError(t *testing.T) {
	t.Parallel()
	exec := &fakeTradeExecutor{err: fmt.Errorf("boom")}
	ws, w, body := newHeldBuyWebServer(t, exec)

	rec := postTrade(t, ws, body)
	if rec.Code == 200 {
		t.Fatalf("status = 200 on executor error, want failure; body: %s", rec.Body.String())
	}
	// The go-registration is only reached past the error return, so a brief
	// wait must never observe a registration.
	if waitWatched(w, "ml-blg", 50*time.Millisecond) {
		t.Error("ml-blg registered despite executor error")
	}
}

func TestHandleTradeNoRegistrationOnUnsuccessfulResult(t *testing.T) {
	t.Parallel()
	exec := &fakeTradeExecutor{result: &polymarket.TradeResult{Success: false, ErrorMsg: "rejected"}}
	ws, w, body := newHeldBuyWebServer(t, exec)

	rec := postTrade(t, ws, body)
	if rec.Code == 200 {
		t.Fatalf("status = 200 on unsuccessful result, want failure; body: %s", rec.Body.String())
	}
	if waitWatched(w, "ml-blg", 50*time.Millisecond) {
		t.Error("ml-blg registered despite result.Success == false")
	}
}
