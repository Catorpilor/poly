package polymarket

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Catorpilor/poly/internal/polymarket/orderv2"
)

// rfqTestKeyHex is a throwaway key; its address is the EOA in take tests.
const rfqTestKeyHex = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

func takeTestQuote() *ComboQuote {
	return &ComboQuote{
		RFQID: "rfq-1", QuoteID: "q-1", Status: "AWAITING_REQUESTER_ACCEPTANCE",
		YesPositionID: "1587172404441704079474252447958188878554308508048282572074770511662550089728",
		MakerAmountE6: 966191, TakerAmountE6: 1932381, TotalRequiredE6: 1000000, NetReceiveE6: 1932381,
	}
}

func takeTestRequest(t *testing.T, accountType string) ComboTakeRequest {
	t.Helper()
	key, err := crypto.HexToECDSA(rfqTestKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	return ComboTakeRequest{
		EOA: crypto.PubkeyToAddress(key.PublicKey), Proxy: rfqTestProxy, AccountType: accountType,
		Creds:      &APICredentials{APIKey: "key-1", Secret: rfqTestSecret, Passphrase: "pass-1"},
		PrivateKey: key, Quote: takeTestQuote(),
	}
}

func TestRequestComboQuote_KeepsQuoteID(t *testing.T) {
	t.Parallel()
	tc := newRFQServer(t, http.StatusOK, rfqAwaitingResponse, nil)
	q, err := tc.RequestComboQuote(context.Background(), testQuoteRequest(string(AccountDepositWallet)))
	if err != nil || q.QuoteID != "q-1" {
		t.Fatalf("quote id = %q, err %v; the accept call needs it", q.QuoteID, err)
	}
}

func TestTakeComboQuote_SignedOrderShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		accountType string
		wantSigType float64
		wantSigner  func(eoa common.Address) common.Address
	}{
		{string(AccountSafe), 2, func(eoa common.Address) common.Address { return eoa }},
		{string(AccountLegacyProxy), 2, func(eoa common.Address) common.Address { return eoa }},
		{string(AccountDepositWallet), 3, func(common.Address) common.Address { return rfqTestProxy }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.accountType, func(t *testing.T) {
			t.Parallel()
			var cap rfqCapture
			tc := newRFQServer(t, http.StatusOK, `{"rfq_id":"rfq-1","status":"EXECUTING","taker_order_hash":"0xabc"}`, &cap)
			req := takeTestRequest(t, tt.accountType)
			before := time.Now().Unix()
			res, err := tc.TakeComboQuote(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Status != "EXECUTING" || res.Terminal() || res.Filled() {
				t.Errorf("result = %+v, want non-terminal EXECUTING", res)
			}

			if cap.method != http.MethodPost || cap.path != "/v1/requester/rfq/requests/rfq-1/accept" {
				t.Errorf("request = %s %s", cap.method, cap.path)
			}
			if cap.header.Get("POLY_ADDRESS") != req.EOA.Hex() || cap.header.Get("POLY_SIGNATURE") == "" {
				t.Errorf("L2 headers missing: %v", cap.header)
			}
			if cap.body["quote_id"] != "q-1" {
				t.Errorf("quote_id = %v", cap.body["quote_id"])
			}
			so, _ := cap.body["signed_order"].(map[string]any)
			if so == nil {
				t.Fatalf("no signed_order in %s", cap.rawBody)
			}
			wantSigner := tt.wantSigner(req.EOA)
			checks := map[string]any{
				"maker": rfqTestProxy.Hex(), "signer": wantSigner.Hex(),
				"tokenId": req.Quote.YesPositionID, "makerAmount": "966191", "takerAmount": "1932381",
				"side": float64(0), "signatureType": tt.wantSigType,
				"metadata": common.Hash{}.Hex(), "builder": common.Hash{}.Hex(),
			}
			for k, want := range checks {
				if got := so[k]; !strings.EqualFold(toString(got), toString(want)) {
					t.Errorf("signed_order.%s = %v, want %v", k, got, want)
				}
			}
			ts, _ := strconv.ParseInt(toString(so["timestamp"]), 10, 64)
			if ts < before || ts > time.Now().Unix()+1 {
				t.Errorf("timestamp = %v, want unix seconds (not ms) near %d", so["timestamp"], before)
			}

			// The signature must be over the Exchange v3 hash of exactly these fields.
			salt, _ := new(big.Int).SetString(toString(so["salt"]), 10)
			tok, _ := new(big.Int).SetString(req.Quote.YesPositionID, 10)
			order := &orderv2.Order{
				Salt: salt, Maker: rfqTestProxy, Signer: wantSigner, TokenId: tok,
				MakerAmount: big.NewInt(966191), TakerAmount: big.NewInt(1932381),
				Side: orderv2.BUY, SignatureType: orderv2.SignatureType(tt.wantSigType), Timestamp: big.NewInt(ts),
			}
			hash, err := orderv2.NewBuilder(137).BuildOrderHash(order, orderv2.ExchangeV3)
			if err != nil {
				t.Fatal(err)
			}
			sig, err := hex.DecodeString(strings.TrimPrefix(toString(so["signature"]), "0x"))
			if err != nil || len(sig) < 65 {
				t.Fatalf("signature = %v (%v)", so["signature"], err)
			}
			if tt.wantSigType == 3 {
				return // ERC-7739 blob; the wrap itself is covered in orderv2 tests
			}
			inner := append([]byte(nil), sig[:65]...)
			if inner[64] >= 27 {
				inner[64] -= 27
			}
			pub, err := crypto.SigToPub(hash.Bytes(), inner)
			if err != nil || crypto.PubkeyToAddress(*pub) != req.EOA {
				t.Errorf("signature does not recover the EOA over the v3 order hash (%v)", err)
			}
		})
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func TestTakeComboQuote_Outcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		status       int
		body         string
		wantErr      bool
		wantStatus   string
		wantTerminal bool
		wantFilled   bool
		wantCode     string
	}{
		{name: "executing", status: 200, body: `{"rfq_id":"rfq-1","status":"EXECUTING"}`, wantStatus: "EXECUTING"},
		{name: "filled immediately", status: 200, body: `{"rfq_id":"rfq-1","status":"FILLED","tx_hash":"0xtx"}`, wantStatus: "FILLED", wantTerminal: true, wantFilled: true},
		{name: "last look decline", status: 200, body: `{"rfq_id":"rfq-1","status":"FAILED","error":{"code":"MAKER_DECLINED","message":"declined"}}`, wantStatus: "FAILED", wantTerminal: true, wantCode: "MAKER_DECLINED"},
		{name: "acceptance window expired", status: 409, body: `{"error":"acceptance window expired","code":"ACCEPTANCE_EXPIRED"}`, wantStatus: "EXPIRED", wantTerminal: true, wantCode: "ACCEPTANCE_EXPIRED"},
		{name: "bad signed order", status: 400, body: `{"error":"invalid signed order","code":"INVALID_SIGNATURE"}`, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newRFQServer(t, tt.status, tt.body, nil)
			res, err := tc.TakeComboQuote(context.Background(), takeTestRequest(t, string(AccountSafe)))
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "INVALID_SIGNATURE") {
					t.Fatalf("err = %v, want INVALID_SIGNATURE", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Status != tt.wantStatus || res.Terminal() != tt.wantTerminal || res.Filled() != tt.wantFilled || res.ErrorCode != tt.wantCode {
				t.Errorf("result = %+v (terminal %v filled %v)", res, res.Terminal(), res.Filled())
			}
		})
	}
}

func TestGetComboRFQStatus(t *testing.T) {
	t.Parallel()

	var cap rfqCapture
	tc := newRFQServer(t, http.StatusOK, `{"rfq_id":"rfq-1","status":"FILLED","tx_hash":"0xtx"}`, &cap)
	res, err := tc.GetComboRFQStatus(context.Background(), rfqTestEOA, &APICredentials{APIKey: "k", Secret: rfqTestSecret, Passphrase: "p"}, "rfq-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/v1/requester/rfq/requests/rfq-1" || cap.rawBody != "" {
		t.Errorf("request = %s %s body %q", cap.method, cap.path, cap.rawBody)
	}
	if !res.Filled() || res.TxHash != "0xtx" {
		t.Errorf("result = %+v", res)
	}
	for _, s := range []string{"CONFIRMED", "FILLED"} {
		if !(&ComboTakeResult{Status: s}).Filled() {
			t.Errorf("%s must count as filled", s)
		}
	}
	for _, s := range []string{"AWAITING_MAKER_CONFIRMATION", "EXECUTING", "MINED", "RETRYING"} {
		if (&ComboTakeResult{Status: s}).Terminal() {
			t.Errorf("%s must be pending, not terminal", s)
		}
	}
}

func TestIsComboRateLimited(t *testing.T) {
	t.Parallel()
	tc := newRFQServer(t, http.StatusTooManyRequests, `{"error":"too many requests","code":"RATE_LIMITED"}`, nil)
	_, err := tc.RequestComboQuote(context.Background(), testQuoteRequest(string(AccountSafe)))
	if !IsComboRateLimited(err) {
		t.Errorf("IsComboRateLimited(%v) = false, want true", err)
	}
	if IsComboRateLimited(errors.New("RATE_LIMITED")) || IsComboRateLimited(nil) {
		t.Error("only a gateway 429 counts")
	}
}
