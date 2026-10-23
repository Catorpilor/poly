package polymarket

import (
	"context"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Catorpilor/poly/internal/polymarket/orderv2"
)

// Shape of a live SELL quote (2026-10-22, $5 combo, not taken).
const rfqSellResponse = `{
  "rfq_id": "rfq-s1",
  "status": "AWAITING_REQUESTER_ACCEPTANCE",
  "expires_at": 1773890763000,
  "request": {"rfq_id": "rfq-s1", "condition_id": "0xcombo", "yes_position_id": "111", "direction": "SELL", "side": "YES",
              "requested_size": {"unit": "shares", "value_e6": "13798319"}},
  "quote": {"quote_id": "qs-1", "blended_price_e6": "339000", "maker_amount_e6": "13798319", "taker_amount_e6": "4677630",
            "total_required_e6": "13798319", "net_receive_e6": "4520000"}
}`

func TestRequestComboQuote_Sell(t *testing.T) {
	t.Parallel()

	var cap rfqCapture
	tc := newRFQServer(t, http.StatusOK, rfqSellResponse, &cap)
	req := testQuoteRequest(string(AccountDepositWallet))
	req.StakeUSD = 0
	req.SellSharesE6 = 13798319
	q, err := tc.RequestComboQuote(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cap.body["direction"] != "SELL" || cap.body["side"] != "YES" {
		t.Errorf("direction/side = %v/%v, want SELL/YES", cap.body["direction"], cap.body["side"])
	}
	size, _ := cap.body["requested_size"].(map[string]any)
	if size["unit"] != "shares" || size["value_e6"] != "13798319" {
		t.Errorf("requested_size = %v, want shares / \"13798319\"", size)
	}
	if q.Direction != "SELL" || q.NoQuote() {
		t.Errorf("quote = %+v", q)
	}
	// Proceeds per share = net_receive / shares sold, after fees.
	if got := q.ProceedsPerShare(); math.Abs(got-4520000.0/13798319.0) > 1e-12 {
		t.Errorf("proceeds/share = %v", got)
	}
	if q.Multiplier() != 0 {
		t.Errorf("a SELL quote has no buy multiplier, got %v", q.Multiplier())
	}
}

func TestRequestComboQuote_RejectsAmbiguousSize(t *testing.T) {
	t.Parallel()
	tc := newRFQServer(t, http.StatusOK, rfqSellResponse, nil)
	req := testQuoteRequest(string(AccountSafe))
	req.SellSharesE6 = 5 // and StakeUSD 10 from the helper
	if _, err := tc.RequestComboQuote(context.Background(), req); err == nil {
		t.Error("a request must be a BUY stake or a SELL size, not both")
	}
}

func TestTakeComboQuote_SellSignsSide1(t *testing.T) {
	t.Parallel()

	var cap rfqCapture
	tc := newRFQServer(t, http.StatusOK, `{"rfq_id":"rfq-s1","status":"FILLED","tx_hash":"0xs"}`, &cap)
	req := takeTestRequest(t, string(AccountSafe))
	req.Quote = &ComboQuote{RFQID: "rfq-s1", QuoteID: "qs-1", Direction: "SELL", Status: "AWAITING_REQUESTER_ACCEPTANCE",
		YesPositionID: "111", MakerAmountE6: 13798319, TakerAmountE6: 4677630, TotalRequiredE6: 13798319, NetReceiveE6: 4520000}
	res, err := tc.TakeComboQuote(context.Background(), req)
	if err != nil || !res.Filled() {
		t.Fatalf("res = %+v, err %v", res, err)
	}
	so, _ := cap.body["signed_order"].(map[string]any)
	if so["side"] != float64(1) || toString(so["makerAmount"]) != "13798319" || toString(so["takerAmount"]) != "4677630" {
		t.Errorf("signed_order = %v, want side 1 with the quote amounts verbatim", so)
	}
	salt, _ := new(big.Int).SetString(toString(so["salt"]), 10)
	ts, _ := strconv.ParseInt(toString(so["timestamp"]), 10, 64)
	hash, _ := orderv2.NewBuilder(137).BuildOrderHash(&orderv2.Order{
		Salt: salt, Maker: rfqTestProxy, Signer: req.EOA, TokenId: big.NewInt(111),
		MakerAmount: big.NewInt(13798319), TakerAmount: big.NewInt(4677630),
		Side: orderv2.SELL, SignatureType: orderv2.POLY_GNOSIS_SAFE, Timestamp: big.NewInt(ts),
	}, orderv2.ExchangeV3)
	sig := common.FromHex(toString(so["signature"]))
	sig[64] -= 27
	pub, err := crypto.SigToPub(hash.Bytes(), sig[:65])
	if err != nil || crypto.PubkeyToAddress(*pub) != req.EOA {
		t.Errorf("signature must recover the EOA over the side-1 v3 hash (%v)", err)
	}
}

func TestComboPosition_SellFields(t *testing.T) {
	t.Parallel()

	page := strings.Replace(liveComboPositionsPage,
		`"leg_index":1,"leg_status":"OPEN"`, `"leg_index":1,"leg_position_id":"LEG1","leg_status":"OPEN"`, 1)
	page = strings.Replace(page, `"leg_index":0,"leg_status":"OPEN"`, `"leg_index":0,"leg_position_id":"LEG0","leg_status":"OPEN"`, 1)
	var gotCondition string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCondition = r.URL.Query().Get("condition")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	pm := NewPositionManagerWithDataAPI(srv.URL)
	c, err := pm.GetComboPosition(context.Background(), common.HexToAddress("0x4b2f"), "0x03136a66e19d923f1296a3291daea378620000000000000000000000000000")
	if err != nil || c == nil {
		t.Fatalf("combo = %v, err %v", c, err)
	}
	if gotCondition != "0x03136a66e19d923f1296a3291daea378620000000000000000000000000000" {
		t.Errorf("condition filter = %q", gotCondition)
	}
	if c.ComboConditionID != "0x03136a66e19d923f1296a3291daea378620000000000000000000000000000" || c.SharesE6 != 13798319 {
		t.Errorf("ids/shares = %q / %d", c.ComboConditionID, c.SharesE6)
	}
	if strings.Join(c.LegPositionIDs(), ",") != "LEG0,LEG1" {
		t.Errorf("leg position ids = %v, want leg order", c.LegPositionIDs())
	}
	if !c.Sellable() {
		t.Error("an open combo with no lost leg is sellable")
	}
}

func TestComboPosition_Sellable(t *testing.T) {
	t.Parallel()

	leg := func(s string) ComboPositionLeg { return ComboPositionLeg{Status: s} }
	tests := []struct {
		name string
		c    ComboPosition
		want bool
	}{
		{"open", ComboPosition{Status: "OPEN", SharesE6: 1, Legs: []ComboPositionLeg{leg("OPEN"), leg("OPEN")}}, true},
		{"partial, a leg won", ComboPosition{Status: "PARTIAL", SharesE6: 1, Legs: []ComboPositionLeg{leg("RESOLVED_WIN"), leg("OPEN")}}, true},
		{"a leg lost", ComboPosition{Status: "PARTIAL", SharesE6: 1, Legs: []ComboPositionLeg{leg("RESOLVED_LOSS"), leg("OPEN")}}, false},
		{"won: redeem, don't sell", ComboPosition{Status: "REDEEMABLE", Redeemable: true, SharesE6: 1, Legs: []ComboPositionLeg{leg("RESOLVED_WIN"), leg("RESOLVED_WIN")}}, false},
		{"no shares", ComboPosition{Status: "OPEN", Legs: []ComboPositionLeg{leg("OPEN"), leg("OPEN")}}, false},
	}
	for _, tt := range tests {
		if got := tt.c.Sellable(); got != tt.want {
			t.Errorf("%s: Sellable = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestGetComboPosition_NotHeld(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"pagination":{"next_cursor":null}}`))
	}))
	defer srv.Close()
	c, err := NewPositionManagerWithDataAPI(srv.URL).GetComboPosition(context.Background(), common.HexToAddress("0x1"), "0xabc")
	if err != nil || c != nil {
		t.Errorf("combo = %v, err %v; want nil, nil when not held", c, err)
	}
}
