package telegram

import (
	"sync"
	"time"
)

// UserState represents the current state of a user's interaction
type UserState string

const (
	StateNone                    UserState = ""
	StateWaitingForKey           UserState = "waiting_for_key"
	StateConfirmImport           UserState = "confirm_import"
	StateWaitingForTrade         UserState = "waiting_for_trade"
	StateWaitingForAmount        UserState = "waiting_for_amount"
	StateSelectingPosition       UserState = "selecting_position"
	StateWaitingForLimitPrice    UserState = "waiting_for_limit_price"      // For sell limit orders
	StateWaitingForBuyLimitPrice UserState = "waiting_for_buy_limit_price" // For buy limit orders
	StateRedeemingPositions      UserState = "redeeming_positions"          // For claim all flow
)

// UserContext holds the context for a user's current interaction
type UserContext struct {
	State     UserState
	Data      map[string]interface{}
	ExpiresAt time.Time
}

// StateManager manages user interaction states
type StateManager struct {
	mu       sync.RWMutex
	states   map[int64]*UserContext
	cleanupInterval time.Duration
}

// NewStateManager creates a new state manager
func NewStateManager() *StateManager {
	sm := &StateManager{
		states:          make(map[int64]*UserContext),
		cleanupInterval: 5 * time.Minute,
	}

	// Start cleanup routine
	go sm.startCleanupRoutine()

	return sm
}

// SetState sets the state for a user
func (sm *StateManager) SetState(userID int64, state UserState, data map[string]interface{}, ttl time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.states[userID] = &UserContext{
		State:     state,
		Data:      data,
		ExpiresAt: time.Now().Add(ttl),
	}
}

// GetState gets the current state for a user
func (sm *StateManager) GetState(userID int64) (*UserContext, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	ctx, exists := sm.states[userID]
	if !exists {
		return nil, false
	}

	// Check if state has expired
	if time.Now().After(ctx.ExpiresAt) {
		// State has expired, remove it
		sm.mu.RUnlock()
		sm.mu.Lock()
		delete(sm.states, userID)
		sm.mu.Unlock()
		sm.mu.RLock()
		return nil, false
	}

	return ctx, true
}

// ClearState clears the state for a user
func (sm *StateManager) ClearState(userID int64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	delete(sm.states, userID)
}

// IsInState checks if a user is in a specific state
func (sm *StateManager) IsInState(userID int64, state UserState) bool {
	ctx, exists := sm.GetState(userID)
	if !exists {
		return false
	}
	return ctx.State == state
}

// startCleanupRoutine periodically removes expired states
func (sm *StateManager) startCleanupRoutine() {
	ticker := time.NewTicker(sm.cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		sm.cleanup()
	}
}

// cleanup removes expired states
func (sm *StateManager) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	for userID, ctx := range sm.states {
		if now.After(ctx.ExpiresAt) {
			delete(sm.states, userID)
		}
	}
}