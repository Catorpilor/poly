package polymarket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// comboRFQURLDefault is Polymarket's Requester gateway for Combo quotes
// (docs.polymarket.com/trading/combos/requesters).
const comboRFQURLDefault = "https://combos-rfq-gateway-requester-api.polymarket.com"

const comboRFQRequestsPath = "/v1/requester/rfq/requests"

// ComboQuoteRequest asks for a Combo Quote: BUY the YES side of the combo
// formed by LegPositionIDs, spending at most StakeUSD (fees included).
type ComboQuoteRequest struct {
	EOA            common.Address // owner of the CLOB API credentials
	Proxy          common.Address // Trading Wallet
	AccountType    string
	Creds          *APICredentials
	LegPositionIDs []string
	StakeUSD       float64
}

// ComboQuote is the gateway's answer to a quote request. A FAILED status
// (e.g. NO_QUOTES) is a normal business outcome, not an error.
type ComboQuote struct {
	RFQID            string
	QuoteID          string
	Status           string
	ExpiresAt        time.Time
	ComboConditionID string
	YesPositionID    string
	BlendedPrice     float64
	MakerAmountE6    int64
	TakerAmountE6    int64
	TotalRequiredE6  int64 // exact collateral needed, fees included
	NetReceiveE6     int64 // combo shares received
	ErrorCode        string
	ErrorMessage     string
}

// NoQuote reports that no executable quote came back.
func (q *ComboQuote) NoQuote() bool {
	return q == nil || q.Status == "FAILED" || q.TotalRequiredE6 <= 0
}

// Multiplier is the fee-inclusive payout per dollar if every leg wins:
// shares received / collateral paid. 0 when there is no quote.
func (q *ComboQuote) Multiplier() float64 {
	if q.NoQuote() || q.NetReceiveE6 <= 0 {
		return 0
	}
	return float64(q.NetReceiveE6) / float64(q.TotalRequiredE6)
}

// comboRequesterIdentity maps an Account Type to the Requester API's
// signer / maker / signature type. The gateway rejects EOA requesters.
//
// Legacy Proxy signs as Safe (2), like the CLOB order path. The Requester
// docs say Proxy = 1, but the live gateway rejects type 1 for these
// accounts with INVALID_IDENTITY (verified 2026-10-22, issue #118).
func comboRequesterIdentity(eoa, proxy common.Address, accountType string) (signer, maker common.Address, sigType int, err error) {
	if proxy == (common.Address{}) || proxy == eoa {
		return common.Address{}, common.Address{}, 0, errors.New("combos need a trading wallet: the RFQ gateway rejects EOA accounts")
	}
	switch AccountType(accountType) {
	case AccountDepositWallet:
		return proxy, proxy, 3, nil
	default: // Safe and Legacy Proxy
		return eoa, proxy, 2, nil
	}
}

type comboRFQCreateBody struct {
	SignerAddress  string           `json:"signer_address"`
	MakerAddress   string           `json:"maker_address"`
	SignatureType  int              `json:"signature_type"`
	LegPositionIDs []string         `json:"leg_position_ids"`
	Direction      string           `json:"direction"`
	Side           string           `json:"side"`
	RequestedSize  comboRFQSizeBody `json:"requested_size"`
}

type comboRFQSizeBody struct {
	Unit    string `json:"unit"`
	ValueE6 string `json:"value_e6"`
}

type comboRFQResponse struct {
	RFQID     string `json:"rfq_id"`
	Status    string `json:"status"`
	ExpiresAt int64  `json:"expires_at"`
	Request   struct {
		ConditionID   string `json:"condition_id"`
		YesPositionID string `json:"yes_position_id"`
	} `json:"request"`
	Quote *struct {
		QuoteID         string `json:"quote_id"`
		BlendedPriceE6  string `json:"blended_price_e6"`
		MakerAmountE6   string `json:"maker_amount_e6"`
		TakerAmountE6   string `json:"taker_amount_e6"`
		TotalRequiredE6 string `json:"total_required_e6"`
		NetReceiveE6    string `json:"net_receive_e6"`
	} `json:"quote"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type comboRFQErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// RequestComboQuote creates a BUY RFQ and returns the winning quote. It
// commits to nothing: a quote expires unless it is Taken within seconds.
// Creates are rate limited (15/min per maker) and not idempotent.
func (tc *TradingClient) RequestComboQuote(ctx context.Context, req ComboQuoteRequest) (*ComboQuote, error) {
	if req.Creds == nil {
		return nil, errors.New("missing API credentials")
	}
	if len(req.LegPositionIDs) < 2 {
		return nil, fmt.Errorf("a combo needs at least 2 legs, got %d", len(req.LegPositionIDs))
	}
	if req.StakeUSD <= 0 {
		return nil, fmt.Errorf("stake must be positive, got %v", req.StakeUSD)
	}
	signer, maker, sigType, err := comboRequesterIdentity(req.EOA, req.Proxy, req.AccountType)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(comboRFQCreateBody{
		SignerAddress:  signer.Hex(),
		MakerAddress:   maker.Hex(),
		SignatureType:  sigType,
		LegPositionIDs: req.LegPositionIDs,
		Direction:      "BUY",
		Side:           "YES",
		RequestedSize: comboRFQSizeBody{
			Unit:    "notional",
			ValueE6: strconv.FormatInt(int64(math.Round(req.StakeUSD*1e6)), 10),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode RFQ: %w", err)
	}

	raw, err := tc.rfqDo(ctx, http.MethodPost, comboRFQRequestsPath, body, req.EOA, req.Creds)
	if err != nil {
		return nil, err
	}

	var r comboRFQResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode RFQ response: %w", err)
	}
	q := &ComboQuote{
		RFQID:            r.RFQID,
		Status:           r.Status,
		ComboConditionID: r.Request.ConditionID,
		YesPositionID:    r.Request.YesPositionID,
	}
	if r.ExpiresAt > 0 {
		q.ExpiresAt = time.UnixMilli(r.ExpiresAt)
	}
	if r.Error != nil {
		q.ErrorCode, q.ErrorMessage = r.Error.Code, r.Error.Message
	}
	if r.Quote != nil {
		q.QuoteID = r.Quote.QuoteID
		q.MakerAmountE6 = parseE6(r.Quote.MakerAmountE6)
		q.TakerAmountE6 = parseE6(r.Quote.TakerAmountE6)
		q.TotalRequiredE6 = parseE6(r.Quote.TotalRequiredE6)
		q.NetReceiveE6 = parseE6(r.Quote.NetReceiveE6)
		q.BlendedPrice = float64(parseE6(r.Quote.BlendedPriceE6)) / 1e6
	}
	return q, nil
}

// parseE6 parses a 6-decimal base-unit integer string; invalid -> 0.
func parseE6(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// rfqHTTPError is a non-200 gateway response (validation, auth, rate limit).
type rfqHTTPError struct {
	Status int
	Code   string
	Msg    string
}

func (e *rfqHTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("RFQ gateway HTTP %d %s: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("RFQ gateway HTTP %d: %s", e.Status, e.Msg)
}

// rfqDo sends one L2-signed request to the Requester gateway and returns the
// 200 body. A GET is signed without a body. Non-200 responses come back as
// *rfqHTTPError so callers can interpret specific statuses (409 = expired).
func (tc *TradingClient) rfqDo(ctx context.Context, method, path string, body []byte, eoa common.Address, creds *APICredentials) ([]byte, error) {
	base := tc.comboRFQURL
	if base == "" {
		base = comboRFQURLDefault
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("create RFQ request: %w", err)
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("POLY_ADDRESS", eoa.Hex())
	httpReq.Header.Set("POLY_API_KEY", creds.APIKey)
	httpReq.Header.Set("POLY_PASSPHRASE", creds.Passphrase)
	httpReq.Header.Set("POLY_TIMESTAMP", timestamp)
	httpReq.Header.Set("POLY_SIGNATURE", tc.signL2Request(creds.Secret, timestamp, method, path, string(body)))

	resp, err := tc.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("RFQ request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read RFQ response: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return raw, nil
	}
	var e comboRFQErrorBody
	_ = json.Unmarshal(raw, &e)
	msg := strings.TrimSpace(e.Error)
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	return nil, &rfqHTTPError{Status: resp.StatusCode, Code: e.Code, Msg: msg}
}

// IsComboRateLimited reports a gateway HTTP 429 (15 quote creates per
// rolling minute per maker address).
func IsComboRateLimited(err error) bool {
	var he *rfqHTTPError
	return errors.As(err, &he) && he.Status == http.StatusTooManyRequests
}
