package reconnect

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// Strategy implements exponential backoff with jitter for reconnection attempts
type Strategy struct {
	minDelay    time.Duration
	maxDelay    time.Duration
	multiplier  float64
	jitter      float64
	attempts    int
	maxAttempts int
	mu          sync.RWMutex

	// Time budget mode (replaces maxAttempts)
	startTime  time.Time
	timeBudget time.Duration
	budgetMode bool
}

// NewStrategy creates a new reconnection strategy with default values
// Uses time budget mode (10 minutes) by default
func NewStrategy() *Strategy {
	return &Strategy{
		minDelay:    1 * time.Second,
		maxDelay:    60 * time.Second,
		multiplier:  2.0,
		jitter:      0.1,
		attempts:    0,
		maxAttempts: 10,
		timeBudget:  10 * time.Minute,
		budgetMode:  true,
		startTime:   time.Now(),
	}
}

// NewCustomStrategy creates a reconnection strategy with custom parameters
func NewCustomStrategy(minDelay, maxDelay time.Duration, multiplier, jitter float64, maxAttempts int) *Strategy {
	return &Strategy{
		minDelay:    minDelay,
		maxDelay:    maxDelay,
		multiplier:  multiplier,
		jitter:      jitter,
		attempts:    0,
		maxAttempts: maxAttempts,
	}
}

// NextDelay calculates the next retry delay with exponential backoff and jitter
// Returns 0 if max attempts reached or time budget exhausted
func (s *Strategy) NextDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check time budget in budget mode
	if s.budgetMode && !s.startTime.IsZero() && time.Since(s.startTime) >= s.timeBudget {
		return 0 // Budget exhausted
	}

	// Check max attempts in non-budget mode
	if !s.budgetMode && s.attempts >= s.maxAttempts {
		return 0 // Stop retrying
	}

	// Calculate exponential delay
	delay := float64(s.minDelay) * math.Pow(s.multiplier, float64(s.attempts))
	delay = math.Min(delay, float64(s.maxDelay))

	// Add random jitter to prevent thundering herd
	jitterAmount := delay * s.jitter * (rand.Float64()*2 - 1)
	finalDelay := time.Duration(delay + jitterAmount)

	s.attempts++
	return finalDelay
}

// Reset resets the retry counter and time budget start time
func (s *Strategy) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = 0
	if s.budgetMode {
		s.startTime = time.Now()
	}
}

// Attempts returns the current number of attempts
func (s *Strategy) Attempts() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.attempts
}

// MaxAttempts returns the maximum number of attempts
func (s *Strategy) MaxAttempts() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxAttempts
}

// HasReachedMax returns true if max attempts reached
func (s *Strategy) HasReachedMax() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.budgetMode {
		return !s.startTime.IsZero() && time.Since(s.startTime) >= s.timeBudget
	}
	return s.attempts >= s.maxAttempts
}

// HasExhaustedBudget returns true if the time budget has been exhausted
func (s *Strategy) HasExhaustedBudget() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.budgetMode {
		return s.attempts >= s.maxAttempts
	}
	return !s.startTime.IsZero() && time.Since(s.startTime) >= s.timeBudget
}

// ResetBudget resets the time budget timer, giving a fresh budget
func (s *Strategy) ResetBudget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startTime = time.Now()
}

// TimeBudget returns the configured time budget duration
func (s *Strategy) TimeBudget() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.timeBudget
}

// Event represents a reconnection event
type Event struct {
	Type      EventType   // Event type
	Attempts  int         // Current attempt number
	Error     error       // Error if any
	Timestamp time.Time   // Event timestamp
	Layer     string      // Layer where event occurred (websocket, session, protocol)
	SessionID string      // Session ID if applicable
	Extra     interface{} // Additional context
}

// EventType represents the type of reconnection event
type EventType string

const (
	EventStarted  EventType = "started"
	EventProgress EventType = "progress"
	EventSuccess  EventType = "success"
	EventFailed   EventType = "failed"
	EventMaxRetry EventType = "max_retry"
	EventAborted  EventType = "aborted"
)

// Callback is called when a reconnection event occurs
type Callback func(event Event)

// CallbackManager manages reconnection event callbacks
type CallbackManager struct {
	callbacks []Callback
	mu        sync.RWMutex
}

// NewCallbackManager creates a new callback manager
func NewCallbackManager() *CallbackManager {
	return &CallbackManager{
		callbacks: make([]Callback, 0),
	}
}

// Subscribe adds a callback
func (m *CallbackManager) Subscribe(callback Callback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbacks = append(m.callbacks, callback)
}

// Notify notifies all callbacks of an event
func (m *CallbackManager) Notify(event Event) {
	m.mu.RLock()
	callbacks := make([]Callback, len(m.callbacks))
	copy(callbacks, m.callbacks)
	m.mu.RUnlock()

	for _, callback := range callbacks {
		go callback(event)
	}
}

// Metrics tracks reconnection statistics
type Metrics struct {
	TotalAttempts      int64
	SuccessfulAttempts int64
	FailedAttempts     int64
	TotalDelay         time.Duration
	MaxDelay           time.Duration
	LastAttemptTime    time.Time
	mu                 sync.RWMutex
}

// NewMetrics creates a new metrics tracker
func NewMetrics() *Metrics {
	return &Metrics{}
}

// RecordAttempt records a reconnection attempt
func (m *Metrics) RecordAttempt(success bool, delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.TotalAttempts++
	if success {
		m.SuccessfulAttempts++
	} else {
		m.FailedAttempts++
	}

	m.TotalDelay += delay
	if delay > m.MaxDelay {
		m.MaxDelay = delay
	}
	m.LastAttemptTime = time.Now()
}

// AverageDelay returns the average reconnection delay
func (m *Metrics) AverageDelay() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.TotalAttempts == 0 {
		return 0
	}
	return m.TotalDelay / time.Duration(m.TotalAttempts)
}

// SuccessRate returns the success rate as a percentage
func (m *Metrics) SuccessRate() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.TotalAttempts == 0 {
		return 0
	}
	return float64(m.SuccessfulAttempts) / float64(m.TotalAttempts) * 100
}

// GetStats returns a snapshot of current metrics
func (m *Metrics) GetStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return map[string]interface{}{
		"total_attempts":      m.TotalAttempts,
		"successful_attempts": m.SuccessfulAttempts,
		"failed_attempts":     m.FailedAttempts,
		"average_delay_ms":    m.AverageDelay().Milliseconds(),
		"max_delay_ms":        m.MaxDelay.Milliseconds(),
		"success_rate":        m.SuccessRate(),
		"last_attempt":        m.LastAttemptTime.Format(time.RFC3339),
	}
}
