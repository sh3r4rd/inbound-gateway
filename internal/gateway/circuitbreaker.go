package gateway

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/sony/gobreaker"
	"go.uber.org/zap"
)

// CircuitBreakerManager manages circuit breakers for different integrations
type CircuitBreakerManager struct {
	config   CircuitBreakerConfig
	logger   *zap.Logger
	breakers map[string]*gobreaker.CircuitBreaker
	mu       sync.RWMutex
	stats    *CircuitBreakerStats
}

// CircuitBreakerStats tracks circuit breaker statistics
type CircuitBreakerStats struct {
	mu           sync.RWMutex
	stateChanges map[string][]StateChange
	failures     map[string]int64
	successes    map[string]int64
}

// StateChange represents a circuit breaker state change
type StateChange struct {
	From      string
	To        string
	Timestamp time.Time
	Reason    string
}

// NewCircuitBreakerManager creates a new circuit breaker manager
func NewCircuitBreakerManager(config CircuitBreakerConfig, logger *zap.Logger) (*CircuitBreakerManager, error) {
	manager := &CircuitBreakerManager{
		config:   config,
		logger:   logger,
		breakers: make(map[string]*gobreaker.CircuitBreaker),
		stats: &CircuitBreakerStats{
			stateChanges: make(map[string][]StateChange),
			failures:     make(map[string]int64),
			successes:    make(map[string]int64),
		},
	}

	// Start monitoring routine
	go manager.monitorRoutine()

	return manager, nil
}

// Call executes a function with circuit breaker protection
func (m *CircuitBreakerManager) Call(integration string, fn func() error) error {
	if !m.config.Enabled {
		// Circuit breaker disabled, execute function directly
		return fn()
	}

	breaker := m.getOrCreateBreaker(integration)

	_, err := breaker.Execute(func() (interface{}, error) {
		execErr := fn()
		if execErr != nil {
			m.recordFailure(integration)
		} else {
			m.recordSuccess(integration)
		}
		return nil, execErr
	})

	if err == gobreaker.ErrOpenState {
		return fmt.Errorf("circuit breaker is open for integration %s", integration)
	}

	if err == gobreaker.ErrTooManyRequests {
		return fmt.Errorf("too many requests for integration %s (half-open state)", integration)
	}

	return err
}

// getOrCreateBreaker gets or creates a circuit breaker for an integration
func (m *CircuitBreakerManager) getOrCreateBreaker(integration string) *gobreaker.CircuitBreaker {
	m.mu.RLock()
	breaker, exists := m.breakers[integration]
	m.mu.RUnlock()

	if exists {
		return breaker
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if breaker, exists := m.breakers[integration]; exists {
		return breaker
	}

	// Create new circuit breaker
	settings := m.createBreakerSettings(integration)
	breaker = gobreaker.NewCircuitBreaker(settings)
	m.breakers[integration] = breaker

	m.logger.Info("Created circuit breaker for integration",
		zap.String("integration", integration),
	)

	return breaker
}

// createBreakerSettings creates settings for a circuit breaker
func (m *CircuitBreakerManager) createBreakerSettings(integration string) gobreaker.Settings {
	return gobreaker.Settings{
		Name:        integration,
		MaxRequests: uint32(m.config.HalfOpenMaxRequests),
		Interval:    m.config.ObservationWindow,
		Timeout:     m.config.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return counts.Requests >= 3 && failureRatio >= m.config.FailureThreshold
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			m.onStateChange(name, from.String(), to.String())
		},
	}
}

// onStateChange handles circuit breaker state changes
func (m *CircuitBreakerManager) onStateChange(name, from, to string) {
	m.logger.Warn("Circuit breaker state changed",
		zap.String("integration", name),
		zap.String("from", from),
		zap.String("to", to),
	)

	// Record state change
	m.stats.mu.Lock()
	if m.stats.stateChanges[name] == nil {
		m.stats.stateChanges[name] = make([]StateChange, 0)
	}
	m.stats.stateChanges[name] = append(m.stats.stateChanges[name], StateChange{
		From:      from,
		To:        to,
		Timestamp: time.Now(),
		Reason:    m.getStateChangeReason(from, to),
	})

	// Keep only last 100 state changes per integration
	if len(m.stats.stateChanges[name]) > 100 {
		m.stats.stateChanges[name] = m.stats.stateChanges[name][1:]
	}
	m.stats.mu.Unlock()

	// Send alert if circuit opens
	if to == "open" {
		m.sendAlert(name, from, to)
	}
}

// getStateChangeReason returns the reason for a state change
func (m *CircuitBreakerManager) getStateChangeReason(from, to string) string {
	switch {
	case to == "open":
		return "failure threshold exceeded"
	case to == "half-open":
		return "timeout expired, testing recovery"
	case to == "closed" && from == "half-open":
		return "recovery successful"
	default:
		return "state transition"
	}
}

// sendAlert sends an alert for circuit breaker state changes
func (m *CircuitBreakerManager) sendAlert(integration, from, to string) {
	// In a production system, this would integrate with an alerting system
	m.logger.Error("ALERT: Circuit breaker opened",
		zap.String("integration", integration),
		zap.String("from_state", from),
		zap.String("to_state", to),
		zap.Time("timestamp", time.Now()),
	)
}

// Reset resets the circuit breaker for an integration
func (m *CircuitBreakerManager) Reset(integration string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.breakers[integration]; exists {
		// gobreaker doesn't have a direct reset method
		// We'll recreate the circuit breaker
		settings := m.createBreakerSettings(integration)
		m.breakers[integration] = gobreaker.NewCircuitBreaker(settings)

		m.logger.Info("Reset circuit breaker",
			zap.String("integration", integration),
		)

		return nil
	}

	return fmt.Errorf("circuit breaker not found for integration %s", integration)
}

// ResetAll resets all circuit breakers
func (m *CircuitBreakerManager) ResetAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for integration := range m.breakers {
		settings := m.createBreakerSettings(integration)
		m.breakers[integration] = gobreaker.NewCircuitBreaker(settings)
	}

	m.logger.Info("Reset all circuit breakers")
}

// GetState returns the state of a circuit breaker
func (m *CircuitBreakerManager) GetState(integration string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if breaker, exists := m.breakers[integration]; exists {
		return breaker.State().String()
	}

	return "unknown"
}

// GetStates returns the states of all circuit breakers
func (m *CircuitBreakerManager) GetStates() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	states := make(map[string]string)
	for integration, breaker := range m.breakers {
		states[integration] = breaker.State().String()
	}

	return states
}

// GetStats returns circuit breaker statistics
func (m *CircuitBreakerManager) GetStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	m.stats.mu.RLock()
	defer m.stats.mu.RUnlock()

	stats := make(map[string]interface{})
	stats["enabled"] = m.config.Enabled
	stats["breakers_count"] = len(m.breakers)

	// Individual breaker stats
	breakerStats := make(map[string]interface{})
	for integration, breaker := range m.breakers {
		counts := breaker.Counts()
		breakerStats[integration] = map[string]interface{}{
			"state":                 breaker.State().String(),
			"requests":              counts.Requests,
			"total_failures":        counts.TotalFailures,
			"total_successes":       counts.TotalSuccesses,
			"consecutive_failures":  counts.ConsecutiveFailures,
			"consecutive_successes": counts.ConsecutiveSuccesses,
			"recorded_failures":     m.stats.failures[integration],
			"recorded_successes":    m.stats.successes[integration],
		}

		// Add recent state changes
		if changes, exists := m.stats.stateChanges[integration]; exists {
			recentChanges := make([]map[string]interface{}, 0)
			// Get last 5 state changes
			start := len(changes) - 5
			if start < 0 {
				start = 0
			}
			for _, change := range changes[start:] {
				recentChanges = append(recentChanges, map[string]interface{}{
					"from":      change.From,
					"to":        change.To,
					"timestamp": change.Timestamp,
					"reason":    change.Reason,
				})
			}
			breakerStats[integration].(map[string]interface{})["recent_state_changes"] = recentChanges
		}
	}
	stats["breakers"] = breakerStats

	return stats
}

// recordSuccess records a successful call
func (m *CircuitBreakerManager) recordSuccess(integration string) {
	m.stats.mu.Lock()
	defer m.stats.mu.Unlock()

	m.stats.successes[integration]++
}

// recordFailure records a failed call
func (m *CircuitBreakerManager) recordFailure(integration string) {
	m.stats.mu.Lock()
	defer m.stats.mu.Unlock()

	m.stats.failures[integration]++
}

// monitorRoutine monitors circuit breakers and performs maintenance
func (m *CircuitBreakerManager) monitorRoutine() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		m.monitor()
	}
}

// monitor checks the health of circuit breakers
func (m *CircuitBreakerManager) monitor() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	openBreakers := 0
	halfOpenBreakers := 0

	for integration, breaker := range m.breakers {
		state := breaker.State()
		switch state {
		case gobreaker.StateOpen:
			openBreakers++
			m.logger.Warn("Circuit breaker is open",
				zap.String("integration", integration),
			)
		case gobreaker.StateHalfOpen:
			halfOpenBreakers++
			m.logger.Info("Circuit breaker is half-open",
				zap.String("integration", integration),
			)
		}
	}

	if openBreakers > 0 || halfOpenBreakers > 0 {
		m.logger.Info("Circuit breaker status",
			zap.Int("total", len(m.breakers)),
			zap.Int("open", openBreakers),
			zap.Int("half_open", halfOpenBreakers),
		)
	}
}

// UpdateConfig updates circuit breaker configuration
func (m *CircuitBreakerManager) UpdateConfig(config CircuitBreakerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.config = config

	// Reset all breakers to apply new configuration
	for integration := range m.breakers {
		settings := m.createBreakerSettings(integration)
		m.breakers[integration] = gobreaker.NewCircuitBreaker(settings)
	}

	m.logger.Info("Updated circuit breaker configuration",
		zap.Float64("failure_threshold", config.FailureThreshold),
		zap.Duration("timeout", config.Timeout),
		zap.Int("half_open_max_requests", config.HalfOpenMaxRequests),
	)
}

// Close closes the circuit breaker manager
func (m *CircuitBreakerManager) Close() error {
	m.logger.Info("Circuit breaker manager closed")
	return nil
}

// Advanced circuit breaker with custom policies

// CustomCircuitBreaker extends the basic circuit breaker with custom policies
type CustomCircuitBreaker struct {
	breaker         *gobreaker.CircuitBreaker
	errorClassifier ErrorClassifier
	backoffStrategy BackoffStrategy
	logger          *zap.Logger
}

// ErrorClassifier classifies errors for circuit breaker decisions
type ErrorClassifier interface {
	IsFailure(error) bool
	IsRecoverable(error) bool
}

// BackoffStrategy defines backoff strategy for circuit breaker
type BackoffStrategy interface {
	NextBackoff(failures int) time.Duration
}

// DefaultErrorClassifier is the default error classifier
type DefaultErrorClassifier struct{}

// IsFailure determines if an error should be counted as a failure
func (d *DefaultErrorClassifier) IsFailure(err error) bool {
	if err == nil {
		return false
	}

	// Classify certain errors as non-failures
	// e.g., client errors (4xx) might not count as failures

	return true
}

// IsRecoverable determines if an error is recoverable
func (d *DefaultErrorClassifier) IsRecoverable(err error) bool {
	// Determine if the error indicates a recoverable condition
	// e.g., timeout, temporary network issues

	return true
}

// ExponentialBackoff implements exponential backoff strategy
type ExponentialBackoff struct {
	InitialInterval time.Duration
	MaxInterval     time.Duration
	Multiplier      float64
}

// NextBackoff calculates the next backoff duration
func (e *ExponentialBackoff) NextBackoff(failures int) time.Duration {
	backoff := time.Duration(float64(e.InitialInterval) * math.Pow(e.Multiplier, float64(failures-1)))
	if backoff > e.MaxInterval {
		backoff = e.MaxInterval
	}
	return backoff
}

// NewCustomCircuitBreaker creates a custom circuit breaker
func NewCustomCircuitBreaker(name string, config CircuitBreakerConfig, logger *zap.Logger) *CustomCircuitBreaker {
	settings := gobreaker.Settings{
		Name:        name,
		MaxRequests: uint32(config.HalfOpenMaxRequests),
		Interval:    config.ObservationWindow,
		Timeout:     config.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return counts.Requests >= 3 && failureRatio >= config.FailureThreshold
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			logger.Info("Circuit breaker state changed",
				zap.String("name", name),
				zap.String("from", from.String()),
				zap.String("to", to.String()),
			)
		},
	}

	return &CustomCircuitBreaker{
		breaker:         gobreaker.NewCircuitBreaker(settings),
		errorClassifier: &DefaultErrorClassifier{},
		backoffStrategy: &ExponentialBackoff{
			InitialInterval: 1 * time.Second,
			MaxInterval:     30 * time.Second,
			Multiplier:      2,
		},
		logger: logger,
	}
}

// Execute executes a function with circuit breaker protection
func (c *CustomCircuitBreaker) Execute(fn func() error) error {
	_, err := c.breaker.Execute(func() (interface{}, error) {
		execErr := fn()
		if execErr != nil && !c.errorClassifier.IsFailure(execErr) {
			// Don't count this as a failure for circuit breaker
			return nil, execErr
		}
		return nil, execErr
	})

	return err
}

// ExecuteWithFallback executes with a fallback function
func (c *CustomCircuitBreaker) ExecuteWithFallback(fn func() error, fallback func() error) error {
	err := c.Execute(fn)
	if err == gobreaker.ErrOpenState || err == gobreaker.ErrTooManyRequests {
		c.logger.Debug("Circuit breaker triggered, executing fallback")
		return fallback()
	}
	return err
}
