package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"go.uber.org/zap"
)

// RateLimiterManager manages rate limiting for the gateway
type RateLimiterManager struct {
	config        RateLimitConfig
	logger        *zap.Logger
	strategies    map[string]RateLimitStrategy
	localLimiters map[string]*rate.Limiter
	redisClient   *redis.Client
	mu            sync.RWMutex
}

// RateLimiter interface for different rate limiting strategies
type RateLimiter interface {
	Allow(ctx context.Context, key string) (bool, error)
	AllowN(ctx context.Context, key string, n int) (bool, error)
	Reset(key string) error
}

// TokenBucketLimiter implements token bucket algorithm
type TokenBucketLimiter struct {
	limiter *rate.Limiter
	mu      sync.Mutex
}

// SlidingWindowLimiter implements sliding window algorithm
type SlidingWindowLimiter struct {
	windowSize   time.Duration
	maxRequests  int
	requests     map[string][]time.Time
	mu           sync.RWMutex
}

// DistributedRateLimiter implements distributed rate limiting using Redis
type DistributedRateLimiter struct {
	redisClient      *redis.Client
	requestsPerSecond float64
	burst            int
	windowSize       time.Duration
	logger           *zap.Logger
}

// NewRateLimiterManager creates a new rate limiter manager
func NewRateLimiterManager(config RateLimitConfig, logger *zap.Logger) (*RateLimiterManager, error) {
	manager := &RateLimiterManager{
		config:        config,
		logger:        logger,
		strategies:    make(map[string]RateLimitStrategy),
		localLimiters: make(map[string]*rate.Limiter),
	}

	// Initialize Redis client if distributed rate limiting is needed
	if config.RedisConfig != nil {
		manager.redisClient = redis.NewClient(&redis.Options{
			Addr:        config.RedisConfig.Addresses[0],
			Password:    config.RedisConfig.Password,
			DB:          config.RedisConfig.DB,
			MaxRetries:  config.RedisConfig.MaxRetries,
			PoolSize:    config.RedisConfig.PoolSize,
			DialTimeout: config.RedisConfig.Timeout,
		})

		// Test Redis connection
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.redisClient.Ping(ctx).Err(); err != nil {
			logger.Warn("Failed to connect to Redis, falling back to local rate limiting", zap.Error(err))
			manager.redisClient = nil
		}
	}

	// Initialize rate limiting strategies
	if err := manager.initializeStrategies(); err != nil {
		return nil, fmt.Errorf("failed to initialize strategies: %w", err)
	}

	// Start cleanup routine
	go manager.cleanupRoutine()

	return manager, nil
}

// initializeStrategies initializes all configured rate limiting strategies
func (m *RateLimiterManager) initializeStrategies() error {
	for _, strategy := range m.config.Strategies {
		switch strategy.Type {
		case "token_bucket":
			m.initializeTokenBucket(strategy)
		case "sliding_window":
			m.initializeSlidingWindow(strategy)
		case "fixed_window":
			m.initializeFixedWindow(strategy)
		default:
			m.logger.Warn("Unknown rate limit strategy type", zap.String("type", strategy.Type))
		}
	}

	return nil
}

// initializeTokenBucket initializes a token bucket strategy
func (m *RateLimiterManager) initializeTokenBucket(strategy RateLimitStrategy) {
	// Token bucket is handled by golang.org/x/time/rate package
	// We'll create limiters on demand in the Allow method
	m.strategies[strategy.Name] = strategy
}

// initializeSlidingWindow initializes a sliding window strategy
func (m *RateLimiterManager) initializeSlidingWindow(strategy RateLimitStrategy) {
	m.strategies[strategy.Name] = strategy
}

// initializeFixedWindow initializes a fixed window strategy
func (m *RateLimiterManager) initializeFixedWindow(strategy RateLimitStrategy) {
	m.strategies[strategy.Name] = strategy
}

// Allow checks if a request is allowed
func (m *RateLimiterManager) Allow(ctx context.Context, integrationType, clientID string) (bool, error) {
	if !m.config.Enabled {
		return true, nil
	}

	// Generate rate limit key
	key := m.generateKey(integrationType, clientID)

	// Check if we should use distributed rate limiting
	if m.redisClient != nil && m.config.PerClient {
		return m.allowDistributed(ctx, key)
	}

	// Use local rate limiting
	return m.allowLocal(ctx, key)
}

// AllowBatch checks if a batch request is allowed
func (m *RateLimiterManager) AllowBatch(ctx context.Context, clientID string) (bool, error) {
	if !m.config.Enabled {
		return true, nil
	}

	// Use different limits for batch operations
	key := fmt.Sprintf("batch:%s", clientID)
	
	// Batch operations typically have lower rate limits
	batchLimit := m.config.RequestsPerSecond / 10 // Example: 10% of normal rate
	
	if m.redisClient != nil {
		return m.allowDistributedWithLimit(ctx, key, batchLimit, m.config.Burst/10)
	}

	return m.allowLocalWithLimit(ctx, key, batchLimit, m.config.Burst/10)
}

// allowLocal checks rate limit using local limiter
func (m *RateLimiterManager) allowLocal(ctx context.Context, key string) (bool, error) {
	m.mu.RLock()
	limiter, exists := m.localLimiters[key]
	m.mu.RUnlock()

	if !exists {
		// Create new limiter
		m.mu.Lock()
		limiter = rate.NewLimiter(rate.Limit(m.config.RequestsPerSecond), m.config.Burst)
		m.localLimiters[key] = limiter
		m.mu.Unlock()
	}

	return limiter.Allow(), nil
}

// allowLocalWithLimit checks rate limit with custom limits
func (m *RateLimiterManager) allowLocalWithLimit(ctx context.Context, key string, limit float64, burst int) (bool, error) {
	m.mu.RLock()
	limiter, exists := m.localLimiters[key]
	m.mu.RUnlock()

	if !exists {
		// Create new limiter with custom limits
		m.mu.Lock()
		limiter = rate.NewLimiter(rate.Limit(limit), burst)
		m.localLimiters[key] = limiter
		m.mu.Unlock()
	}

	return limiter.Allow(), nil
}

// allowDistributed checks rate limit using Redis
func (m *RateLimiterManager) allowDistributed(ctx context.Context, key string) (bool, error) {
	return m.allowDistributedWithLimit(ctx, key, m.config.RequestsPerSecond, m.config.Burst)
}

// allowDistributedWithLimit checks distributed rate limit with custom limits
func (m *RateLimiterManager) allowDistributedWithLimit(ctx context.Context, key string, limit float64, burst int) (bool, error) {
	// Implement distributed rate limiting using Redis
	// Using sliding window algorithm with Redis

	now := time.Now()
	windowStart := now.Add(-time.Second) // 1-second window
	
	pipe := m.redisClient.TxPipeline()
	
	// Remove old entries
	pipe.ZRemRangeByScore(ctx, key, "0", fmt.Sprintf("%d", windowStart.UnixNano()))

	// Count current entries
	_ = pipe.ZCount(ctx, key, fmt.Sprintf("%d", windowStart.UnixNano()), fmt.Sprintf("%d", now.UnixNano()))

	// Add current request
	pipe.ZAdd(ctx, key, &redis.Z{
		Score:  float64(now.UnixNano()),
		Member: fmt.Sprintf("%d-%s", now.UnixNano(), uuid.New().String()),
	})
	
	// Set expiry
	pipe.Expire(ctx, key, 2*time.Second)
	
	// Execute pipeline
	cmds, err := pipe.Exec(ctx)
	if err != nil {
		m.logger.Error("Redis pipeline error", zap.Error(err))
		// Fail open - allow request on Redis error
		return true, nil
	}
	
	// Check count from the second command (ZCount)
	if len(cmds) >= 2 {
		if countCmd, ok := cmds[1].(*redis.IntCmd); ok {
			currentCount, _ := countCmd.Result()
			if currentCount >= int64(limit) {
				// Rate limit exceeded
				return false, nil
			}
		}
	}
	
	return true, nil
}

// generateKey generates a rate limit key
func (m *RateLimiterManager) generateKey(integrationType, clientID string) string {
	if m.config.PerClient && m.config.PerIntegration {
		return fmt.Sprintf("rl:%s:%s", integrationType, clientID)
	} else if m.config.PerClient {
		return fmt.Sprintf("rl:client:%s", clientID)
	} else if m.config.PerIntegration {
		return fmt.Sprintf("rl:integration:%s", integrationType)
	}
	return "rl:global"
}

// Reset resets rate limit for a key
func (m *RateLimiterManager) Reset(key string) error {
	// Reset local limiter
	m.mu.Lock()
	if limiter, exists := m.localLimiters[key]; exists {
		// Reset by creating new limiter
		m.localLimiters[key] = rate.NewLimiter(limiter.Limit(), limiter.Burst())
	}
	m.mu.Unlock()

	// Reset in Redis if applicable
	if m.redisClient != nil {
		ctx := context.Background()
		if err := m.redisClient.Del(ctx, key).Err(); err != nil {
			return fmt.Errorf("failed to reset rate limit in Redis: %w", err)
		}
	}

	return nil
}

// ResetAll resets all rate limits
func (m *RateLimiterManager) ResetAll() error {
	// Reset all local limiters
	m.mu.Lock()
	m.localLimiters = make(map[string]*rate.Limiter)
	m.mu.Unlock()

	// Reset in Redis if applicable
	if m.redisClient != nil {
		ctx := context.Background()
		// Use pattern to delete all rate limit keys
		iter := m.redisClient.Scan(ctx, 0, "rl:*", 0).Iterator()
		for iter.Next(ctx) {
			if err := m.redisClient.Del(ctx, iter.Val()).Err(); err != nil {
				m.logger.Error("Failed to delete key", zap.String("key", iter.Val()), zap.Error(err))
			}
		}
		if err := iter.Err(); err != nil {
			return fmt.Errorf("failed to scan Redis keys: %w", err)
		}
	}

	return nil
}

// UpdateLimits updates rate limit configuration
func (m *RateLimiterManager) UpdateLimits(requestsPerSecond float64, burst int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.config.RequestsPerSecond = requestsPerSecond
	m.config.Burst = burst

	// Update existing limiters
	for key := range m.localLimiters {
		newLimiter := rate.NewLimiter(rate.Limit(requestsPerSecond), burst)
		m.localLimiters[key] = newLimiter

		m.logger.Info("Updated rate limiter",
			zap.String("key", key),
			zap.Float64("requests_per_second", requestsPerSecond),
			zap.Int("burst", burst),
		)
	}
}

// GetLimiterStats returns statistics about rate limiters
func (m *RateLimiterManager) GetLimiterStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := make(map[string]interface{})
	stats["enabled"] = m.config.Enabled
	stats["requests_per_second"] = m.config.RequestsPerSecond
	stats["burst"] = m.config.Burst
	stats["active_limiters"] = len(m.localLimiters)
	stats["redis_enabled"] = m.redisClient != nil

	// Get individual limiter stats
	limiters := make(map[string]interface{})
	for key, limiter := range m.localLimiters {
		limiters[key] = map[string]interface{}{
			"limit":  limiter.Limit(),
			"burst":  limiter.Burst(),
			"tokens": limiter.Tokens(), // Available tokens
		}
	}
	stats["limiters"] = limiters

	return stats
}

// cleanupRoutine periodically cleans up unused rate limiters
func (m *RateLimiterManager) cleanupRoutine() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		m.cleanup()
	}
}

// cleanup removes inactive rate limiters
func (m *RateLimiterManager) cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// For simplicity, we'll clear limiters that have full token buckets
	// In production, you might want to track last usage time
	toDelete := make([]string, 0)
	
	for key, limiter := range m.localLimiters {
		// If limiter has all tokens available, it hasn't been used recently
		if limiter.Tokens() >= float64(limiter.Burst()) {
			toDelete = append(toDelete, key)
		}
	}

	for _, key := range toDelete {
		delete(m.localLimiters, key)
	}

	if len(toDelete) > 0 {
		m.logger.Debug("Cleaned up unused rate limiters", zap.Int("count", len(toDelete)))
	}
}

// Close closes the rate limiter manager
func (m *RateLimiterManager) Close() error {
	if m.redisClient != nil {
		if err := m.redisClient.Close(); err != nil {
			return fmt.Errorf("failed to close Redis client: %w", err)
		}
	}
	
	m.logger.Info("Rate limiter manager closed")
	return nil
}

// SlidingWindowRateLimiter implementation

// NewSlidingWindowLimiter creates a new sliding window rate limiter
func NewSlidingWindowLimiter(windowSize time.Duration, maxRequests int) *SlidingWindowLimiter {
	return &SlidingWindowLimiter{
		windowSize:  windowSize,
		maxRequests: maxRequests,
		requests:    make(map[string][]time.Time),
	}
}

// Allow checks if a request is allowed
func (s *SlidingWindowLimiter) Allow(ctx context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-s.windowSize)

	// Get existing requests for this key
	timestamps, exists := s.requests[key]
	if !exists {
		timestamps = make([]time.Time, 0)
	}

	// Remove old requests outside the window
	validTimestamps := make([]time.Time, 0)
	for _, ts := range timestamps {
		if ts.After(windowStart) {
			validTimestamps = append(validTimestamps, ts)
		}
	}

	// Check if we're under the limit
	if len(validTimestamps) >= s.maxRequests {
		s.requests[key] = validTimestamps
		return false, nil
	}

	// Add current request
	validTimestamps = append(validTimestamps, now)
	s.requests[key] = validTimestamps

	return true, nil
}

// AllowN checks if n requests are allowed
func (s *SlidingWindowLimiter) AllowN(ctx context.Context, key string, n int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-s.windowSize)

	// Get existing requests for this key
	timestamps, exists := s.requests[key]
	if !exists {
		timestamps = make([]time.Time, 0)
	}

	// Remove old requests outside the window
	validTimestamps := make([]time.Time, 0)
	for _, ts := range timestamps {
		if ts.After(windowStart) {
			validTimestamps = append(validTimestamps, ts)
		}
	}

	// Check if we have room for n requests
	if len(validTimestamps)+n > s.maxRequests {
		s.requests[key] = validTimestamps
		return false, nil
	}

	// Add n requests
	for i := 0; i < n; i++ {
		validTimestamps = append(validTimestamps, now)
	}
	s.requests[key] = validTimestamps

	return true, nil
}

// Reset resets the limiter for a key
func (s *SlidingWindowLimiter) Reset(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	delete(s.requests, key)
	return nil
}
