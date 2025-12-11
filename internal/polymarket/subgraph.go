package polymarket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// SubgraphClient interacts with Polymarket's GraphQL API
type SubgraphClient struct {
	endpoint   string
	httpClient *http.Client
}

// NewSubgraphClient creates a new subgraph client
func NewSubgraphClient() *SubgraphClient {
	return &SubgraphClient{
		// Polymarket's subgraph on The Graph
		endpoint: "https://api.thegraph.com/subgraphs/name/polymarket/matic-markets",
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// MarketInfo contains market details from the subgraph
type MarketInfo struct {
	ID           string `json:"id"`
	Question     string `json:"question"`
	ConditionID  string `json:"conditionId"`
	QuestionID   string `json:"questionId"`
	Outcomes     []string `json:"outcomes"`
	Category     string `json:"category"`
	Volume       string `json:"volume"`
	Liquidity    string `json:"liquidity"`
	EndDate      string `json:"endDate"`
	YesTokenID   string `json:"yesTokenId"`
	NoTokenID    string `json:"noTokenId"`
}

// UserPosition represents a user's position from the subgraph
type UserPosition struct {
	ID          string `json:"id"`
	Market      *MarketInfo `json:"market"`
	User        string `json:"user"`
	YesShares   string `json:"yesShares"`
	NoShares    string `json:"noShares"`
	NetShares   string `json:"netShares"`
}

// GetUserPositions queries the subgraph for user positions
func (sc *SubgraphClient) GetUserPositions(ctx context.Context, userAddress common.Address) ([]*UserPosition, error) {
	query := `
	query UserPositions($user: String!) {
		positions(where: {user: $user, netShares_gt: "0"}) {
			id
			user
			yesShares
			noShares
			netShares
			market {
				id
				question
				conditionId
				questionId
				outcomes
				category
				volume
				liquidity
				endDate
			}
		}
	}`

	variables := map[string]interface{}{
		"user": strings.ToLower(userAddress.Hex()),
	}

	result, err := sc.query(ctx, query, variables)
	if err != nil {
		return nil, err
	}

	var response struct {
		Positions []*UserPosition `json:"positions"`
	}

	if err := json.Unmarshal(result, &response); err != nil {
		return nil, fmt.Errorf("failed to decode positions: %w", err)
	}

	return response.Positions, nil
}

// GetMarketByConditionID fetches market info by condition ID
func (sc *SubgraphClient) GetMarketByConditionID(ctx context.Context, conditionID string) (*MarketInfo, error) {
	query := `
	query MarketByCondition($conditionId: String!) {
		markets(where: {conditionId: $conditionId}) {
			id
			question
			conditionId
			questionId
			outcomes
			category
			volume
			liquidity
			endDate
		}
	}`

	variables := map[string]interface{}{
		"conditionId": strings.ToLower(conditionID),
	}

	result, err := sc.query(ctx, query, variables)
	if err != nil {
		return nil, err
	}

	var response struct {
		Markets []*MarketInfo `json:"markets"`
	}

	if err := json.Unmarshal(result, &response); err != nil {
		return nil, fmt.Errorf("failed to decode market: %w", err)
	}

	if len(response.Markets) == 0 {
		return nil, fmt.Errorf("market not found for condition ID: %s", conditionID)
	}

	return response.Markets[0], nil
}

// GetActiveMarkets fetches currently active markets
func (sc *SubgraphClient) GetActiveMarkets(ctx context.Context, limit int) ([]*MarketInfo, error) {
	query := `
	query ActiveMarkets($limit: Int!) {
		markets(
			first: $limit,
			orderBy: volume,
			orderDirection: desc,
			where: {resolved: false}
		) {
			id
			question
			conditionId
			questionId
			outcomes
			category
			volume
			liquidity
			endDate
		}
	}`

	variables := map[string]interface{}{
		"limit": limit,
	}

	result, err := sc.query(ctx, query, variables)
	if err != nil {
		return nil, err
	}

	var response struct {
		Markets []*MarketInfo `json:"markets"`
	}

	if err := json.Unmarshal(result, &response); err != nil {
		return nil, fmt.Errorf("failed to decode markets: %w", err)
	}

	return response.Markets, nil
}

// GetUserTradedMarkets fetches markets where user has traded
func (sc *SubgraphClient) GetUserTradedMarkets(ctx context.Context, userAddress common.Address) ([]*MarketInfo, error) {
	query := `
	query UserMarkets($user: String!) {
		userMarkets: positions(where: {user: $user}) {
			market {
				id
				question
				conditionId
				questionId
				outcomes
				category
				volume
				liquidity
				endDate
			}
		}
	}`

	variables := map[string]interface{}{
		"user": strings.ToLower(userAddress.Hex()),
	}

	result, err := sc.query(ctx, query, variables)
	if err != nil {
		return nil, err
	}

	var response struct {
		UserMarkets []struct {
			Market *MarketInfo `json:"market"`
		} `json:"userMarkets"`
	}

	if err := json.Unmarshal(result, &response); err != nil {
		return nil, fmt.Errorf("failed to decode user markets: %w", err)
	}

	// Extract unique markets
	marketMap := make(map[string]*MarketInfo)
	for _, um := range response.UserMarkets {
		if um.Market != nil {
			marketMap[um.Market.ID] = um.Market
		}
	}

	markets := []*MarketInfo{}
	for _, market := range marketMap {
		markets = append(markets, market)
	}

	return markets, nil
}

// query executes a GraphQL query
func (sc *SubgraphClient) query(ctx context.Context, query string, variables map[string]interface{}) (json.RawMessage, error) {
	payload := map[string]interface{}{
		"query":     query,
		"variables": variables,
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", sc.endpoint, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := sc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL error: %s", result.Errors[0].Message)
	}

	return result.Data, nil
}