package polymarket

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const rfqTestSecret = "c2VjcmV0LWtleS1mb3ItdGVzdHM=" // base64("secret-key-for-tests")

var (
	rfqTestEOA   = common.HexToAddress("0x1111111111111111111111111111111111111111")
	rfqTestProxy = common.HexToAddress("0x2222222222222222222222222222222222222222")
)

// Shape from docs.polymarket.com/trading/combos/requesters ("Create RFQ Response").
const rfqAwaitingResponse = `{
  "rfq_id": "rfq-1",
  "status": "AWAITING_REQUESTER_ACCEPTANCE",
  "expires_at": 1773890763000,
  "request": {
    "rfq_id": "rfq-1",
    "condition_id": "0xcombo",
    "yes_position_id": "111",
    "no_position_id": "222",
    "direction": "BUY",
    "side": "YES",
    "requested_size": {"unit": "notional", "value_e6": "1000000"},
    "created_at": 1773890758000
  },
  "quote": {
    "quote_id": "q-1",
    "blended_price_e6": "500000",
    "maker_amount_e6": "966191",
    "taker_amount_e6": "1932381",
    "total_required_e6": "1000000",
    "net_receive_e6": "1932381"
  }
}`

type rfqCapture struct {
	method, path string
	header       http.Header
	body         map[string]any
	rawBody      string
}

func newRFQServer(t *testing.T, status int, response string, cap *rfqCapture) *TradingClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if cap != nil {
			cap.method, cap.path, cap.header, cap.rawBody = r.Method, r.URL.Path, r.Header.Clone(), string(raw)
			_ = json.Unmarshal(raw, &cap.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	tc := NewTradingClient("http://clob.invalid", 137)
	tc.comboRFQURL = srv.URL
	return tc
}

func testQuoteRequest(accountType string) ComboQuoteRequest {
	return ComboQuoteRequest{
		EOA:            rfqTestEOA,
		Proxy:          rfqTestProxy,
		AccountType:    accountType,
		Creds:          &APICredentials{APIKey: "key-1", Secret: rfqTestSecret, Passphrase: "pass-1"},
		LegPositionIDs: []string{"111111", "222222"},
		StakeUSD:       10,
	}
}

func TestRequestComboQuote_RequestShapeAndAuth(t *testing.T) {
	t.Parallel()

	var cap rfqCapture
	tc := newRFQServer(t, http.StatusOK, rfqAwaitingResponse, &cap)
	if _, err := tc.RequestComboQuote(context.Background(), testQuoteRequest(string(AccountDepositWallet))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cap.method != http.MethodPost || cap.path != "/v1/requester/rfq/requests" {
		t.Errorf("request = %s %s, want POST /v1/requester/rfq/requests", cap.method, cap.path)
	}
	if got := cap.header.Get("POLY_ADDRESS"); got != rfqTestEOA.Hex() {
		t.Errorf("POLY_ADDRESS = %q, want the credential EOA %q", got, rfqTestEOA.Hex())
	}
	if cap.header.Get("POLY_API_KEY") != "key-1" || cap.header.Get("POLY_PASSPHRASE") != "pass-1" {
		t.Errorf("api key/passphrase headers wrong: %v", cap.header)
	}
	// Signature = base64url(HMAC-SHA256(b64decode(secret), ts+METHOD+path+body)).
	secret, _ := base64.StdEncoding.DecodeString(rfqTestSecret)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(cap.header.Get("POLY_TIMESTAMP") + "POST" + "/v1/requester/rfq/requests" + cap.rawBody))
	if want := base64.URLEncoding.EncodeToString(mac.Sum(nil)); cap.header.Get("POLY_SIGNATURE") != want {
		t.Errorf("POLY_SIGNATURE = %q, want %q", cap.header.Get("POLY_SIGNATURE"), want)
	}

	if cap.body["direction"] != "BUY" || cap.body["side"] != "YES" {
		t.Errorf("direction/side = %v/%v, want BUY/YES", cap.body["direction"], cap.body["side"])
	}
	legs, _ := cap.body["leg_position_ids"].([]any)
	if len(legs) != 2 || legs[0] != "111111" || legs[1] != "222222" {
		t.Errorf("leg_position_ids = %v", cap.body["leg_position_ids"])
	}
	size, _ := cap.body["requested_size"].(map[string]any)
	if size["unit"] != "notional" || size["value_e6"] != "10000000" {
		t.Errorf("requested_size = %v, want notional / \"10000000\"", size)
	}
}

func TestRequestComboQuote_RequesterIdentityByAccountType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		accountType          string
		wantSigner, wantMake common.Address
		wantSigType          float64
	}{
		{string(AccountDepositWallet), rfqTestProxy, rfqTestProxy, 3},
		{string(AccountSafe), rfqTestEOA, rfqTestProxy, 2},
		{string(AccountLegacyProxy), rfqTestEOA, rfqTestProxy, 2}, // live: type 1 -> INVALID_IDENTITY (2026-10-22)
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.accountType, func(t *testing.T) {
			t.Parallel()
			var cap rfqCapture
			tc := newRFQServer(t, http.StatusOK, rfqAwaitingResponse, &cap)
			if _, err := tc.RequestComboQuote(context.Background(), testQuoteRequest(tt.accountType)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.EqualFold(cap.body["signer_address"].(string), tt.wantSigner.Hex()) ||
				!strings.EqualFold(cap.body["maker_address"].(string), tt.wantMake.Hex()) ||
				cap.body["signature_type"] != tt.wantSigType {
				t.Errorf("signer/maker/type = %v/%v/%v, want %s/%s/%v",
					cap.body["signer_address"], cap.body["maker_address"], cap.body["signature_type"],
					tt.wantSigner.Hex(), tt.wantMake.Hex(), tt.wantSigType)
			}
		})
	}
}

func TestRequestComboQuote_RejectsEOAAccounts(t *testing.T) {
	t.Parallel()

	tc := newRFQServer(t, http.StatusOK, rfqAwaitingResponse, nil)
	req := testQuoteRequest(string(AccountSafe))
	req.Proxy = common.Address{}
	if _, err := tc.RequestComboQuote(context.Background(), req); err == nil || !strings.Contains(err.Error(), "trading wallet") {
		t.Errorf("err = %v, want a trading-wallet-required error (the gateway rejects EOA requesters)", err)
	}
}

func TestRequestComboQuote_ParsesQuote(t *testing.T) {
	t.Parallel()

	tc := newRFQServer(t, http.StatusOK, rfqAwaitingResponse, nil)
	q, err := tc.RequestComboQuote(context.Background(), testQuoteRequest(string(AccountDepositWallet)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q.RFQID != "rfq-1" || q.Status != "AWAITING_REQUESTER_ACCEPTANCE" || q.NoQuote() {
		t.Errorf("quote = %+v", q)
	}
	if q.ComboConditionID != "0xcombo" || q.YesPositionID != "111" {
		t.Errorf("combo ids = %q / %q", q.ComboConditionID, q.YesPositionID)
	}
	if !q.ExpiresAt.Equal(time.UnixMilli(1773890763000)) {
		t.Errorf("expires = %v", q.ExpiresAt)
	}
	if q.TotalRequiredE6 != 1000000 || q.NetReceiveE6 != 1932381 || q.MakerAmountE6 != 966191 || q.TakerAmountE6 != 1932381 {
		t.Errorf("amounts = %+v", q)
	}
	// Docs example: pay $1.00, receive 1.932381 shares -> x1.93 fee-inclusive.
	if math.Abs(q.Multiplier()-1.932381) > 1e-9 {
		t.Errorf("multiplier = %v, want 1.932381", q.Multiplier())
	}
	if math.Abs(q.BlendedPrice-0.5) > 1e-12 {
		t.Errorf("blended price = %v, want 0.5", q.BlendedPrice)
	}
}

func TestRequestComboQuote_NoQuotesIsABusinessOutcome(t *testing.T) {
	t.Parallel()

	tc := newRFQServer(t, http.StatusOK, `{"rfq_id":"rfq-2","status":"FAILED","error":{"code":"NO_QUOTES","message":"no quotes"}}`, nil)
	q, err := tc.RequestComboQuote(context.Background(), testQuoteRequest(string(AccountDepositWallet)))
	if err != nil {
		t.Fatalf("NO_QUOTES must not be a transport error: %v", err)
	}
	if !q.NoQuote() || q.ErrorCode != "NO_QUOTES" || q.Multiplier() != 0 {
		t.Errorf("quote = %+v", q)
	}
}

func TestRequestComboQuote_HTTPErrorCarriesCode(t *testing.T) {
	t.Parallel()

	tc := newRFQServer(t, http.StatusBadRequest, `{"error":"invalid RFQ body","code":"INVALID_LEGS"}`, nil)
	_, err := tc.RequestComboQuote(context.Background(), testQuoteRequest(string(AccountDepositWallet)))
	if err == nil || !strings.Contains(err.Error(), "INVALID_LEGS") || !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want HTTP 400 with code INVALID_LEGS", err)
	}
}
