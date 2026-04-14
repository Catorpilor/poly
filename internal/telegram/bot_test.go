package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// newFakeTelegramServer returns an httptest.Server that responds to getMe
// with a valid bot user, allowing BotAPI initialization to succeed.
func newFakeTelegramServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tgbotapi.APIResponse{
			Ok: true,
			Result: mustMarshal(t, tgbotapi.User{
				ID:        123,
				IsBot:     true,
				FirstName: "TestBot",
				UserName:  "test_bot",
			}),
		})
	}))
}

func mustMarshal(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustMarshal: %v", err)
	}
	return b
}

func TestNewBotAPIWithClient_HasTimeout(t *testing.T) {
	t.Parallel()
	server := newFakeTelegramServer(t)
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 75 * time.Second}
	bot, err := tgbotapi.NewBotAPIWithClient("fake-token", server.URL+"/bot%s/%s", client)
	if err != nil {
		t.Fatalf("NewBotAPIWithClient failed: %v", err)
	}

	httpClient, ok := bot.Client.(*http.Client)
	if !ok {
		t.Fatal("bot.Client is not *http.Client")
	}
	if httpClient.Timeout != 75*time.Second {
		t.Errorf("expected timeout 75s, got %v", httpClient.Timeout)
	}
}

func TestHTTPClientTimeout_CancelsHungRequest(t *testing.T) {
	t.Parallel()

	// Server that never responds — simulates the network hang that caused the bug.
	hung := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hung // block forever
	}))
	t.Cleanup(func() {
		close(hung)
		server.Close()
	})

	client := &http.Client{Timeout: 100 * time.Millisecond}

	start := time.Now()
	req, err := http.NewRequest("GET", server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	_, err = client.Do(req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Should return within ~200ms (100ms timeout + margin), not hang.
	if elapsed > 1*time.Second {
		t.Errorf("request took %v, expected ~100ms timeout", elapsed)
	}
}
