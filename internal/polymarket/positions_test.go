package polymarket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestGetRedeemablePositions_Empty(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify query params
		if r.URL.Query().Get("redeemable") != "true" {
			t.Errorf("expected redeemable=true param, got %q", r.URL.Query().Get("redeemable"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]DataAPIPosition{})
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(nil, "", server.URL)
	positions, err := pm.GetRedeemablePositions(context.Background(), common.HexToAddress("0x1234"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("expected 0 positions, got %d", len(positions))
	}
}

func TestGetRedeemablePositions_StandardMarket(t *testing.T) {
	t.Parallel()

	apiPositions := []DataAPIPosition{
		{
			Title:        "Will X happen?",
			Outcome:      "Yes",
			ConditionID:  "0xabc123",
			Asset:        "token-yes-id",
			OppositeAsset: "token-no-id",
			Size:         50.0,
			CurPrice:     1.0,
			Redeemable:   true,
			NegativeRisk: false,
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiPositions)
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(nil, "", server.URL)
	positions, err := pm.GetRedeemablePositions(context.Background(), common.HexToAddress("0x1234"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}

	pos := positions[0]
	if pos.Title != "Will X happen?" {
		t.Errorf("title = %q, want %q", pos.Title, "Will X happen?")
	}
	if pos.Outcome != "Yes" {
		t.Errorf("outcome = %q, want %q", pos.Outcome, "Yes")
	}
	if pos.ConditionID != "0xabc123" {
		t.Errorf("conditionID = %q, want %q", pos.ConditionID, "0xabc123")
	}
	if pos.Size != 50.0 {
		t.Errorf("size = %f, want 50.0", pos.Size)
	}
	if pos.EstPayout != 50.0 {
		t.Errorf("estPayout = %f, want 50.0", pos.EstPayout)
	}
	if pos.NegativeRisk {
		t.Error("expected NegativeRisk = false")
	}
}

func TestGetRedeemablePositions_NegRiskMarket(t *testing.T) {
	t.Parallel()

	apiPositions := []DataAPIPosition{
		{
			Title:         "Multi-outcome market",
			Outcome:       "No",
			ConditionID:   "0xdef456",
			Asset:         "token-no",
			OppositeAsset: "token-yes",
			Size:          25.0,
			CurPrice:      1.0,
			Redeemable:    true,
			NegativeRisk:  true,
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiPositions)
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(nil, "", server.URL)
	positions, err := pm.GetRedeemablePositions(context.Background(), common.HexToAddress("0x1234"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}

	if !positions[0].NegativeRisk {
		t.Error("expected NegativeRisk = true")
	}
}

func TestGetRedeemablePositions_FiltersZeroSize(t *testing.T) {
	t.Parallel()

	apiPositions := []DataAPIPosition{
		{
			Title:       "Active position",
			ConditionID: "0x111",
			Size:        10.0,
			CurPrice:    1.0,
			Redeemable:  true,
		},
		{
			Title:       "Zero-size position",
			ConditionID: "0x222",
			Size:        0,
			CurPrice:    1.0,
			Redeemable:  true,
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiPositions)
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(nil, "", server.URL)
	positions, err := pm.GetRedeemablePositions(context.Background(), common.HexToAddress("0x1234"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 1 {
		t.Errorf("expected 1 position (zero-size filtered), got %d", len(positions))
	}
}

func TestGetRedeemablePositions_IncludesLosingPositions(t *testing.T) {
	t.Parallel()

	// Losing positions (CurPrice=0) should NOT be filtered out for redeemable positions
	apiPositions := []DataAPIPosition{
		{
			Title:       "Winning position",
			ConditionID: "0x111",
			Size:        10.0,
			CurPrice:    1.0,
			Redeemable:  true,
		},
		{
			Title:       "Losing position",
			ConditionID: "0x222",
			Size:        5.0,
			CurPrice:    0.0,
			Redeemable:  true,
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiPositions)
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(nil, "", server.URL)
	positions, err := pm.GetRedeemablePositions(context.Background(), common.HexToAddress("0x1234"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both should be included — losing positions still part of redemption
	if len(positions) != 2 {
		t.Errorf("expected 2 positions (including losing), got %d", len(positions))
	}
}

func TestGetRedeemablePositions_URLFormat(t *testing.T) {
	t.Parallel()

	proxyAddr := common.HexToAddress("0xAbCdEf1234567890AbCdEf1234567890AbCdEf12")

	var requestURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]DataAPIPosition{})
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(nil, "", server.URL)
	pm.GetRedeemablePositions(context.Background(), proxyAddr)

	expectedPath := "/positions"
	if r := requestURL; r == "" {
		t.Fatal("no request made")
	}

	// Should contain the path and redeemable param
	if requestURL != expectedPath+"?redeemable=true&user=0xabcdef1234567890abcdef1234567890abcdef12" &&
		requestURL != expectedPath+"?user=0xabcdef1234567890abcdef1234567890abcdef12&redeemable=true" {
		t.Errorf("unexpected URL: %s", requestURL)
	}
}

func TestGetRedeemablePositions_HTTPError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(nil, "", server.URL)
	_, err := pm.GetRedeemablePositions(context.Background(), common.HexToAddress("0x1234"))
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}
