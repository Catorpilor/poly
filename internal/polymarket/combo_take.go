package polymarket

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/Catorpilor/poly/internal/polymarket/orderv2"
)

// ComboTakeRequest Takes a Combo Quote: sign the requester order on
// Exchange v3 and submit it to the quote's RFQ.
type ComboTakeRequest struct {
	EOA         common.Address
	Proxy       common.Address
	AccountType string
	Creds       *APICredentials
	PrivateKey  *ecdsa.PrivateKey // the EOA's key
	Quote       *ComboQuote
}

// ComboTakeResult is an RFQ's state after a Take (or a status poll).
type ComboTakeResult struct {
	RFQID          string
	Status         string
	TxHash         string
	TakerOrderHash string
	ErrorCode      string
	ErrorMessage   string
}

// Filled reports a confirmed fill (the only success, see ADR 0010).
func (r *ComboTakeResult) Filled() bool {
	return r != nil && (r.Status == "FILLED" || r.Status == "CONFIRMED")
}

// Terminal reports a final status. Anything else — AWAITING_MAKER_CONFIRMATION,
// EXECUTING, MINED, RETRYING, or unknown — is a Pending Take, never a failure.
func (r *ComboTakeResult) Terminal() bool {
	if r == nil {
		return false
	}
	switch r.Status {
	case "FILLED", "CONFIRMED", "FAILED", "EXPIRED", "CANCELED":
		return true
	}
	return false
}

type comboSignedOrderBody struct {
	Salt          string `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Side          int    `json:"side"`
	SignatureType int    `json:"signatureType"`
	Timestamp     string `json:"timestamp"`
	Metadata      string `json:"metadata"`
	Builder       string `json:"builder"`
	Signature     string `json:"signature"`
}

type comboAcceptBody struct {
	QuoteID     string               `json:"quote_id"`
	SignedOrder comboSignedOrderBody `json:"signed_order"`
}

type comboRFQStateResponse struct {
	RFQID          string `json:"rfq_id"`
	Status         string `json:"status"`
	TxHash         string `json:"tx_hash"`
	TakerOrderHash string `json:"taker_order_hash"`
	Error          *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TakeComboQuote signs the BUY requester order for a quote and accepts it.
// The order uses the quote's amounts verbatim, tokenId = the combo YES
// position id, builder = zero, and a unix-seconds timestamp. An HTTP 409
// (acceptance window expired) is a terminal EXPIRED result, not an error:
// nothing executed, so the caller may re-quote.
func (tc *TradingClient) TakeComboQuote(ctx context.Context, req ComboTakeRequest) (*ComboTakeResult, error) {
	q := req.Quote
	if q == nil || q.NoQuote() || q.RFQID == "" || q.QuoteID == "" || q.YesPositionID == "" {
		return nil, errors.New("no takeable quote")
	}
	if req.Creds == nil || req.PrivateKey == nil {
		return nil, errors.New("missing credentials or signing key")
	}
	signer, maker, sigType, err := comboRequesterIdentity(req.EOA, req.Proxy, req.AccountType)
	if err != nil {
		return nil, err
	}

	signed, err := tc.builder.BuildSignedOrder(req.PrivateKey, &orderv2.OrderData{
		Maker:         maker.Hex(),
		Signer:        signer.Hex(),
		TokenId:       q.YesPositionID,
		MakerAmount:   strconv.FormatInt(q.MakerAmountE6, 10),
		TakerAmount:   strconv.FormatInt(q.TakerAmountE6, 10),
		Side:          orderv2.BUY,
		SignatureType: orderv2.SignatureType(sigType),
		Timestamp:     strconv.FormatInt(time.Now().Unix(), 10),
	}, orderv2.ExchangeV3)
	if err != nil {
		return nil, fmt.Errorf("sign combo order: %w", err)
	}
	o := signed.Order
	body, err := json.Marshal(comboAcceptBody{
		QuoteID: q.QuoteID,
		SignedOrder: comboSignedOrderBody{
			Salt:          o.Salt.String(),
			Maker:         o.Maker.Hex(),
			Signer:        o.Signer.Hex(),
			TokenID:       o.TokenId.String(),
			MakerAmount:   o.MakerAmount.String(),
			TakerAmount:   o.TakerAmount.String(),
			Side:          int(o.Side),
			SignatureType: int(o.SignatureType),
			Timestamp:     o.Timestamp.String(),
			Metadata:      o.Metadata.Hex(),
			Builder:       o.Builder.Hex(),
			Signature:     hexutil.Encode(signed.Signature),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode accept: %w", err)
	}

	path := comboRFQRequestsPath + "/" + url.PathEscape(q.RFQID) + "/accept"
	raw, err := tc.rfqDo(ctx, http.MethodPost, path, body, req.EOA, req.Creds)
	if err != nil {
		var he *rfqHTTPError
		if errors.As(err, &he) && he.Status == http.StatusConflict {
			return &ComboTakeResult{RFQID: q.RFQID, Status: "EXPIRED", ErrorCode: he.Code, ErrorMessage: he.Msg}, nil
		}
		return nil, err
	}
	return parseComboRFQState(raw, q.RFQID)
}

// GetComboRFQStatus reads an RFQ's durable state after a Take.
func (tc *TradingClient) GetComboRFQStatus(ctx context.Context, eoa common.Address, creds *APICredentials, rfqID string) (*ComboTakeResult, error) {
	if creds == nil {
		return nil, errors.New("missing API credentials")
	}
	raw, err := tc.rfqDo(ctx, http.MethodGet, comboRFQRequestsPath+"/"+url.PathEscape(rfqID), nil, eoa, creds)
	if err != nil {
		return nil, err
	}
	return parseComboRFQState(raw, rfqID)
}

func parseComboRFQState(raw []byte, rfqID string) (*ComboTakeResult, error) {
	var r comboRFQStateResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode RFQ state: %w", err)
	}
	res := &ComboTakeResult{RFQID: r.RFQID, Status: r.Status, TxHash: r.TxHash, TakerOrderHash: r.TakerOrderHash}
	if res.RFQID == "" {
		res.RFQID = rfqID
	}
	if r.Error != nil {
		res.ErrorCode, res.ErrorMessage = r.Error.Code, r.Error.Message
	}
	return res, nil
}
