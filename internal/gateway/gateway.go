package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
)

// Metrics for monitoring
var (
	requestCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "inbound_gateway_requests_total",
			Help: "Total number of requests processed by the inbound gateway",
		},
		[]string{"integration_type", "status", "method"},
	)

	requestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "inbound_gateway_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"integration_type", "method"},
	)

	activeConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "inbound_gateway_active_connections",
			Help: "Number of active connections",
		},
	)
)

// InboundGateway is the main service struct
type InboundGateway struct {
	config         *Config
	validator      *RequestValidator
	router         *MessageRouter
	publisher      *PubSubPublisher
	rateLimiter    *RateLimiterManager
	circuitBreaker *CircuitBreakerManager
	authenticator  *Authenticator
	logger         *zap.Logger
	server         *http.Server
	mu             sync.RWMutex
	shutdownCh     chan struct{}
}

// Config holds the configuration for the gateway
type Config struct {
	Port                    string
	ProjectID               string
	MaxRequestSize          int64
	RequestTimeout          time.Duration
	ShutdownTimeout         time.Duration
	EnableMetrics           bool
	EnableTracing           bool
	RateLimitConfig         RateLimitConfig
	CircuitBreakerConfig    CircuitBreakerConfig
	PubSubConfig           PubSubConfig
	AuthConfig             AuthConfig
	IntegrationConfigs     map[string]*IntegrationConfig
}

// IntegrationConfig defines configuration for each integration type
type IntegrationConfig struct {
	Name                string                 `json:"name"`
	Type                string                 `json:"type"` // rest, graphql, webhook, message_queue
	Enabled             bool                   `json:"enabled"`
	ValidationRules     ValidationRules        `json:"validation_rules"`
	RoutingRules        RoutingRules          `json:"routing_rules"`
	RateLimitOverride   *RateLimitConfig       `json:"rate_limit_override,omitempty"`
	CircuitBreakerOverride *CircuitBreakerConfig `json:"circuit_breaker_override,omitempty"`
	TransformationRules *TransformationRules   `json:"transformation_rules,omitempty"`
	AuthRequirements    AuthRequirements       `json:"auth_requirements"`
	Metadata            map[string]interface{} `json:"metadata"`
}

// ValidationRules defines validation rules for incoming requests
type ValidationRules struct {
	RequiredFields  []string               `json:"required_fields"`
	FieldValidators map[string]FieldValidator `json:"field_validators"`
	MaxPayloadSize  int64                  `json:"max_payload_size"`
	ContentTypes    []string               `json:"content_types"`
	CustomRules     []CustomValidationRule `json:"custom_rules"`
}

// FieldValidator defines validation for specific fields
type FieldValidator struct {
	Type       string      `json:"type"` // string, number, boolean, object, array
	Required   bool        `json:"required"`
	MinLength  *int        `json:"min_length,omitempty"`
	MaxLength  *int        `json:"max_length,omitempty"`
	Pattern    string      `json:"pattern,omitempty"`
	Min        *float64    `json:"min,omitempty"`
	Max        *float64    `json:"max,omitempty"`
	Enum       []string    `json:"enum,omitempty"`
	CustomFunc string      `json:"custom_func,omitempty"`
}

// CustomValidationRule for complex validation logic
type CustomValidationRule struct {
	Name        string `json:"name"`
	Expression  string `json:"expression"` // CEL expression or custom function name
	ErrorMessage string `json:"error_message"`
}

// RoutingRules defines how messages are routed
type RoutingRules struct {
	DefaultTopic    string                    `json:"default_topic"`
	ConditionalRoutes []ConditionalRoute      `json:"conditional_routes"`
	HeaderBasedRouting map[string]string      `json:"header_based_routing"`
	Partitioning    *PartitioningConfig      `json:"partitioning,omitempty"`
}

// ConditionalRoute defines conditional routing logic
type ConditionalRoute struct {
	Condition  string `json:"condition"` // CEL expression
	Topic      string `json:"topic"`
	Priority   int    `json:"priority"`
}

// PartitioningConfig for message partitioning
type PartitioningConfig struct {
	Enabled       bool   `json:"enabled"`
	PartitionKey  string `json:"partition_key"`
	NumPartitions int    `json:"num_partitions"`
}

// TransformationRules for data transformation
type TransformationRules struct {
	Enabled      bool                     `json:"enabled"`
	Transformers []TransformerConfig      `json:"transformers"`
}

// TransformerConfig defines a transformation step
type TransformerConfig struct {
	Name        string                 `json:"name"`
	Type        string                 `json:"type"` // jq, template, custom
	Config      map[string]interface{} `json:"config"`
	Order       int                    `json:"order"`
}

// AuthRequirements defines authentication requirements
type AuthRequirements struct {
	Enabled       bool     `json:"enabled"`
	Methods       []string `json:"methods"` // api_key, jwt, oauth2, mtls
	Required      bool     `json:"required"`
	AllowedScopes []string `json:"allowed_scopes,omitempty"`
	AllowedRoles  []string `json:"allowed_roles,omitempty"`
}

// RateLimitConfig for rate limiting
type RateLimitConfig struct {
	Enabled           bool              `json:"enabled"`
	RequestsPerSecond float64           `json:"requests_per_second"`
	Burst             int               `json:"burst"`
	PerClient         bool              `json:"per_client"`
	PerIntegration    bool              `json:"per_integration"`
	ClientIdentifier  string            `json:"client_identifier"` // header, ip, token
	RedisConfig       *RedisConfig      `json:"redis_config,omitempty"`
	Strategies        []RateLimitStrategy `json:"strategies"`
}

// RateLimitStrategy defines different rate limiting strategies
type RateLimitStrategy struct {
	Name      string                 `json:"name"`
	Type      string                 `json:"type"` // token_bucket, sliding_window, fixed_window
	Config    map[string]interface{} `json:"config"`
	Priority  int                    `json:"priority"`
}

// CircuitBreakerConfig for circuit breaker pattern
type CircuitBreakerConfig struct {
	Enabled             bool          `json:"enabled"`
	FailureThreshold    float64       `json:"failure_threshold"`
	SuccessThreshold    float64       `json:"success_threshold"`
	Timeout             time.Duration `json:"timeout"`
	HalfOpenMaxRequests int           `json:"half_open_max_requests"`
	ObservationWindow   time.Duration `json:"observation_window"`
}

// PubSubConfig for Pub/Sub configuration
type PubSubConfig struct {
	ProjectID            string        `json:"project_id"`
	TopicPrefix          string        `json:"topic_prefix"`
	EnableMessageOrdering bool          `json:"enable_message_ordering"`
	EnableRetries        bool          `json:"enable_retries"`
	RetryConfig          *RetryConfig  `json:"retry_config,omitempty"`
	PublishTimeout       time.Duration `json:"publish_timeout"`
	BatchingEnabled      bool          `json:"batching_enabled"`
	BatchingConfig       *BatchingConfig `json:"batching_config,omitempty"`
}

// RetryConfig for retry logic
type RetryConfig struct {
	MaxRetries       int           `json:"max_retries"`
	InitialDelay     time.Duration `json:"initial_delay"`
	MaxDelay         time.Duration `json:"max_delay"`
	Multiplier       float64       `json:"multiplier"`
	RandomizationFactor float64    `json:"randomization_factor"`
}

// BatchingConfig for message batching
type BatchingConfig struct {
	MaxMessages      int           `json:"max_messages"`
	MaxBytes         int           `json:"max_bytes"`
	MaxLatency       time.Duration `json:"max_latency"`
}

// AuthConfig for authentication configuration
type AuthConfig struct {
	JWTConfig     *JWTConfig     `json:"jwt_config,omitempty"`
	APIKeyConfig  *APIKeyConfig  `json:"api_key_config,omitempty"`
	OAuth2Config  *OAuth2Config  `json:"oauth2_config,omitempty"`
	MTLSConfig    *MTLSConfig    `json:"mtls_config,omitempty"`
}

// RedisConfig for Redis configuration
type RedisConfig struct {
	Addresses  []string      `json:"addresses"`
	Password   string        `json:"password"`
	DB         int           `json:"db"`
	MaxRetries int           `json:"max_retries"`
	PoolSize   int           `json:"pool_size"`
	TLS        bool          `json:"tls"`
	Timeout    time.Duration `json:"timeout"`
}

// Request represents an incoming request
type Request struct {
	ID              string                 `json:"id"`
	IntegrationType string                 `json:"integration_type"`
	Method          string                 `json:"method"`
	Path            string                 `json:"path"`
	Headers         map[string][]string    `json:"headers"`
	QueryParams     map[string][]string    `json:"query_params"`
	Body            json.RawMessage        `json:"body,omitempty"`
	ClientID        string                 `json:"client_id"`
	Timestamp       time.Time              `json:"timestamp"`
	TraceID         string                 `json:"trace_id"`
	SpanID          string                 `json:"span_id"`
	Metadata        map[string]interface{} `json:"metadata"`
}

// Response represents the gateway response
type Response struct {
	ID        string                 `json:"id"`
	Status    string                 `json:"status"` // success, error, partial
	Message   string                 `json:"message,omitempty"`
	Data      interface{}            `json:"data,omitempty"`
	Errors    []Error                `json:"errors,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
	TraceID   string                 `json:"trace_id,omitempty"`
}

// Error represents an error response
type Error struct {
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details,omitempty"`
	Field   string                 `json:"field,omitempty"`
}

// NewInboundGateway creates a new instance of InboundGateway
func NewInboundGateway(config *Config) (*InboundGateway, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize logger: %w", err)
	}

	gateway := &InboundGateway{
		config:     config,
		logger:     logger,
		shutdownCh: make(chan struct{}),
	}

	// Initialize components
	if err := gateway.initializeComponents(); err != nil {
		return nil, fmt.Errorf("failed to initialize components: %w", err)
	}

	return gateway, nil
}

// initializeComponents initializes all gateway components
func (g *InboundGateway) initializeComponents() error {
	var err error

	// Initialize validator
	g.validator, err = NewRequestValidator(g.config, g.logger)
	if err != nil {
		return fmt.Errorf("failed to initialize validator: %w", err)
	}

	// Initialize router
	g.router, err = NewMessageRouter(g.config, g.logger)
	if err != nil {
		return fmt.Errorf("failed to initialize router: %w", err)
	}

	// Initialize publisher
	g.publisher, err = NewPubSubPublisher(g.config.PubSubConfig, g.logger)
	if err != nil {
		return fmt.Errorf("failed to initialize publisher: %w", err)
	}

	// Initialize rate limiter
	g.rateLimiter, err = NewRateLimiterManager(g.config.RateLimitConfig, g.logger)
	if err != nil {
		return fmt.Errorf("failed to initialize rate limiter: %w", err)
	}

	// Initialize circuit breaker
	g.circuitBreaker, err = NewCircuitBreakerManager(g.config.CircuitBreakerConfig, g.logger)
	if err != nil {
		return fmt.Errorf("failed to initialize circuit breaker: %w", err)
	}

	// Initialize authenticator
	g.authenticator, err = NewAuthenticator(&g.config.AuthConfig, g.logger)
	if err != nil {
		return fmt.Errorf("failed to initialize authenticator: %w", err)
	}

	return nil
}

// Start starts the gateway server
func (g *InboundGateway) Start(ctx context.Context) error {
	g.logger.Info("Starting Inbound Gateway", zap.String("port", g.config.Port))

	// Setup HTTP server
	router := g.SetupRoutes()
	
	g.server = &http.Server{
		Addr:         ":" + g.config.Port,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Start server in goroutine
	go func() {
		if err := g.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			g.logger.Fatal("Failed to start server", zap.Error(err))
		}
	}()

	// Start background workers
	g.startBackgroundWorkers(ctx)

	g.logger.Info("Inbound Gateway started successfully", zap.String("port", g.config.Port))
	return nil
}

// Stop gracefully stops the gateway
func (g *InboundGateway) Stop(ctx context.Context) error {
	g.logger.Info("Stopping Inbound Gateway")

	// Signal shutdown
	close(g.shutdownCh)

	// Shutdown HTTP server
	shutdownCtx, cancel := context.WithTimeout(ctx, g.config.ShutdownTimeout)
	defer cancel()

	if err := g.server.Shutdown(shutdownCtx); err != nil {
		g.logger.Error("Failed to shutdown server gracefully", zap.Error(err))
		return err
	}

	// Close all components
	if err := g.closeComponents(); err != nil {
		g.logger.Error("Failed to close components", zap.Error(err))
		return err
	}

	g.logger.Info("Inbound Gateway stopped successfully")
	return nil
}

// closeComponents closes all gateway components
func (g *InboundGateway) closeComponents() error {
	var errs []error

	if g.publisher != nil {
		if err := g.publisher.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close publisher: %w", err))
		}
	}

	if g.rateLimiter != nil {
		if err := g.rateLimiter.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close rate limiter: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors closing components: %v", errs)
	}

	return nil
}

// startBackgroundWorkers starts background maintenance workers
func (g *InboundGateway) startBackgroundWorkers(ctx context.Context) {
	// Metrics reporter
	go g.metricsReporter(ctx)

	// Health checker
	go g.healthChecker(ctx)

	// Config reloader
	go g.configReloader(ctx)
}

// metricsReporter periodically reports metrics
func (g *InboundGateway) metricsReporter(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-g.shutdownCh:
			return
		case <-ticker.C:
			g.reportMetrics()
		}
	}
}

// reportMetrics reports current metrics
func (g *InboundGateway) reportMetrics() {
	// Report custom metrics
	g.logger.Debug("Reporting metrics",
		zap.Float64("active_connections", getGaugeValue(activeConnections)),
	)
}

// healthChecker performs health checks
func (g *InboundGateway) healthChecker(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-g.shutdownCh:
			return
		case <-ticker.C:
			g.performHealthCheck()
		}
	}
}

// performHealthCheck checks the health of all components
func (g *InboundGateway) performHealthCheck() {
	// Check publisher health
	if g.publisher != nil && !g.publisher.IsHealthy() {
		g.logger.Warn("Publisher is unhealthy")
	}

	// Check circuit breaker states
	if g.circuitBreaker != nil {
		states := g.circuitBreaker.GetStates()
		for integration, state := range states {
			if state == "open" {
				g.logger.Warn("Circuit breaker is open", zap.String("integration", integration))
			}
		}
	}
}

// configReloader handles dynamic config reloading
func (g *InboundGateway) configReloader(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-g.shutdownCh:
			return
		case <-ticker.C:
			g.reloadConfig()
		}
	}
}

// reloadConfig reloads configuration
func (g *InboundGateway) reloadConfig() {
	// Implementation for dynamic config reloading
	// This could fetch from Firestore, ConfigMaps, etc.
	g.logger.Debug("Checking for configuration updates")
}

// Helper function to get gauge value
func getGaugeValue(gauge prometheus.Gauge) float64 {
	metric := &dto.Metric{}
	gauge.Write(metric)
	return metric.Gauge.GetValue()
}
