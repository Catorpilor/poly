package polymarket

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TradeExecutor bundles the per-trade ceremony that every entry point
// otherwise repeats by hand: API credentials, an L2 auth pre-check, fee
// discovery, then ExecuteTrade. The web trade path uses it today; the
// Telegram executors predate it and can migrate onto it later.
type TradeExecutor struct {
	trading *TradingClient
	markets *MarketClient
}

func NewTradeExecutor(trading *TradingClient, markets *MarketClient) *TradeExecutor {
	return &TradeExecutor{trading: trading, markets: markets}
}

// Execute submits an already-resolved trade request: req must carry the
// market/token identity, side, and amount; Execute fills TakerFeeBps,
// CalcFeeBps and NegativeRisk before submission.
//
// Credential or L2-auth failures abort before any order reaches the CLOB —
// a stale credential should fail loudly here, not as a rejected order. Fee
// discovery is best-effort: a missing fee schedule must not block a trade.
func (e *TradeExecutor) Execute(
	ctx context.Context,
	privateKey *ecdsa.PrivateKey,
	proxyAddress common.Address,
	req *TradeRequest,
) (*TradeResult, error) {
	creds, err := e.trading.GetOrCreateAPICredentials(ctx, privateKey)
	if err != nil {
		return nil, fmt.Errorf("get API credentials: %w", err)
	}

	eoaAddress := crypto.PubkeyToAddress(privateKey.PublicKey)
	if err := e.trading.TestL2Auth(ctx, eoaAddress, creds); err != nil {
		return nil, fmt.Errorf("L2 auth check: %w", err)
	}

	// Get fee rates and negRisk: Gamma feeSchedule for calculation, CLOB
	// for order submission (the three API layers disagree — see
	// docs/ARCHITECTURE.md fee model notes)
	if gammaMarket, err := e.markets.GetMarketByID(ctx, req.MarketID); err != nil {
		log.Printf("TradeExecutor: Gamma market lookup failed: %v (using defaults)", err)
	} else {
		req.CalcFeeBps = gammaMarket.GetFeeRateBps()
		req.NegativeRisk = gammaMarket.NegRisk
	}
	if feeRate, err := e.trading.GetFeeRate(ctx, req.TokenID); err != nil {
		log.Printf("TradeExecutor: CLOB fee-rate lookup failed: %v (using 0)", err)
	} else {
		req.TakerFeeBps = feeRate
	}

	return e.trading.ExecuteTrade(ctx, privateKey, proxyAddress, creds, req)
}
