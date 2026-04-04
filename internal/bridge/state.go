package bridge

import (
	"sync"
	"time"
)

// ConnectionState represents the current state of the WebSocket connection
type ConnectionState int

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateFailed
)

// String returns the string representation of the connection state
func (s ConnectionState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// StateManager manages connection state transitions
type StateManager struct {
	state             ConnectionState
	lastTransition    time.Time
	transitionHistory []StateTransition
	mu                sync.RWMutex
	maxHistory        int
}

// StateTransition records a state change
type StateTransition struct {
	From      ConnectionState
	To        ConnectionState
	Timestamp time.Time
	Reason    string
}

// NewStateManager creates a new state manager
func NewStateManager() *StateManager {
	return &StateManager{
		state:             StateDisconnected,
		lastTransition:    time.Now(),
		transitionHistory: make([]StateTransition, 0, 100),
		maxHistory:        100,
	}
}

// GetState returns the current connection state
func (sm *StateManager) GetState() ConnectionState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state
}

// SetState transitions to a new state
func (sm *StateManager) SetState(newState ConnectionState, reason string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.state == newState {
		return
	}

	transition := StateTransition{
		From:      sm.state,
		To:        newState,
		Timestamp: time.Now(),
		Reason:    reason,
	}

	sm.state = newState
	sm.lastTransition = transition.Timestamp

	// Add to history
	sm.transitionHistory = append(sm.transitionHistory, transition)
	if len(sm.transitionHistory) > sm.maxHistory {
		sm.transitionHistory = sm.transitionHistory[1:]
	}
}

// IsConnected returns true if the connection is in a connected state
func (sm *StateManager) IsConnected() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state == StateConnected
}

// CanReconnect returns true if reconnection is allowed from current state
func (sm *StateManager) CanReconnect() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state == StateDisconnected || sm.state == StateFailed
}

// GetHistory returns the state transition history
func (sm *StateManager) GetHistory() []StateTransition {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	history := make([]StateTransition, len(sm.transitionHistory))
	copy(history, sm.transitionHistory)
	return history
}

// GetLastTransitionTime returns when the last state transition occurred
func (sm *StateManager) GetLastTransitionTime() time.Time {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.lastTransition
}

// GetStateInfo returns detailed state information
func (sm *StateManager) GetStateInfo() map[string]interface{} {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	return map[string]interface{}{
		"current_state":    sm.state.String(),
		"last_transition":  sm.lastTransition.Format(time.RFC3339),
		"time_in_state":    time.Since(sm.lastTransition).String(),
		"transition_count": len(sm.transitionHistory),
	}
}
