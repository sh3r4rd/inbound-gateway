package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opencensus.io/trace"
	"go.uber.org/zap"
)

// SetupRoutes configures all HTTP routes and middleware
func (g *InboundGateway) SetupRoutes() *mux.Router {
	router := mux.NewRouter()

	// Apply global middleware
	router.Use(g.tracingMiddleware)
	router.Use(g.loggingMiddleware)
	router.Use(g.recoveryMiddleware)
	router.Use(g.metricsMiddleware)
	router.Use(g.corsMiddleware)
	router.Use(g.requestIDMiddleware)
	router.Use(g.timeoutMiddleware)

	// Health and metrics endpoints
	router.HandleFunc("/health", g.handleHealth).Methods("GET")
	router.HandleFunc("/ready", g.handleReady).Methods("GET")
	router.Handle("/metrics", promhttp.Handler()).Methods("GET")

	// API versioning
	v1Router := router.PathPrefix("/v1").Subrouter()

	// Integration endpoints
	integrationRouter := v1Router.PathPrefix("/integrations").Subrouter()
	integrationRouter.Use(g.authenticationMiddleware)
	integrationRouter.Use(g.rateLimitMiddleware)
	integrationRouter.Use(g.circuitBreakerMiddleware)

	// Dynamic routing based on integration type
	integrationRouter.HandleFunc("/{integrationType}", g.handleIntegration).Methods("POST", "PUT", "PATCH")
	integrationRouter.HandleFunc("/{integrationType}/{id}", g.handleIntegration).Methods("GET", "DELETE")
	
	// Webhook endpoints
	webhookRouter := v1Router.PathPrefix("/webhooks").Subrouter()
	webhookRouter.Use(g.webhookAuthMiddleware)
	webhookRouter.HandleFunc("/{provider}", g.handleWebhook).Methods("POST")

	// GraphQL endpoint
	v1Router.Handle("/graphql", g.graphqlHandler()).Methods("POST")

	// Batch operations
	batchRouter := v1Router.PathPrefix("/batch").Subrouter()
	batchRouter.Use(g.authenticationMiddleware)
	batchRouter.Use(g.batchRateLimitMiddleware)
	batchRouter.HandleFunc("/upload", g.handleBatchUpload).Methods("POST")
	batchRouter.HandleFunc("/status/{batchId}", g.handleBatchStatus).Methods("GET")

	// Admin endpoints
	adminRouter := router.PathPrefix("/admin").Subrouter()
	adminRouter.Use(g.adminAuthMiddleware)
	adminRouter.HandleFunc("/config/reload", g.handleConfigReload).Methods("POST")
	adminRouter.HandleFunc("/circuit-breaker/reset/{integrationType}", g.handleCircuitBreakerReset).Methods("POST")
	adminRouter.HandleFunc("/rate-limit/update", g.handleRateLimitUpdate).Methods("POST")

	return router
}

// Request context keys
type contextKey string

const (
	contextKeyRequestID    contextKey = "request_id"
	contextKeyTraceID      contextKey = "trace_id"
	contextKeySpanID       contextKey = "span_id"
	contextKeyClientID     contextKey = "client_id"
	contextKeyIntegration  contextKey = "integration"
	contextKeyStartTime    contextKey = "start_time"
	contextKeyAuthClaims   contextKey = "auth_claims"
)

// tracingMiddleware adds distributed tracing
func (g *InboundGateway) tracingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := trace.StartSpan(r.Context(), fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		defer span.End()

		// Extract trace context from headers
		traceID := r.Header.Get("X-Trace-Id")
		if traceID == "" {
			traceID = uuid.New().String()
		}

		spanID := r.Header.Get("X-Span-Id")
		if spanID == "" {
			spanID = uuid.New().String()
		}

		// Add trace context
		ctx = context.WithValue(ctx, contextKeyTraceID, traceID)
		ctx = context.WithValue(ctx, contextKeySpanID, spanID)

		// Add trace headers to response
		w.Header().Set("X-Trace-Id", traceID)
		w.Header().Set("X-Span-Id", spanID)

		// Add span attributes
		span.AddAttributes(
			trace.StringAttribute("http.method", r.Method),
			trace.StringAttribute("http.path", r.URL.Path),
			trace.StringAttribute("http.user_agent", r.UserAgent()),
			trace.StringAttribute("trace.id", traceID),
		)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// loggingMiddleware logs all requests
func (g *InboundGateway) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Log request
		g.logger.Info("Request received",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.String("remote_addr", r.RemoteAddr),
			zap.String("user_agent", r.UserAgent()),
			zap.String("trace_id", getTraceID(r.Context())),
		)

		next.ServeHTTP(wrapped, r)

		// Log response
		duration := time.Since(start)
		g.logger.Info("Request completed",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", wrapped.statusCode),
			zap.Duration("duration", duration),
			zap.String("trace_id", getTraceID(r.Context())),
		)
	})
}

// recoveryMiddleware recovers from panics
func (g *InboundGateway) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				g.logger.Error("Panic recovered",
					zap.Any("error", err),
					zap.String("path", r.URL.Path),
					zap.String("trace_id", getTraceID(r.Context())),
				)

				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// metricsMiddleware collects metrics
func (g *InboundGateway) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		activeConnections.Inc()
		defer activeConnections.Dec()

		next.ServeHTTP(wrapped, r)

		// Record metrics
		duration := time.Since(start).Seconds()
		integrationType := mux.Vars(r)["integrationType"]
		if integrationType == "" {
			integrationType = "unknown"
		}

		requestCounter.WithLabelValues(
			integrationType,
			fmt.Sprintf("%d", wrapped.statusCode),
			r.Method,
		).Inc()

		requestDuration.WithLabelValues(
			integrationType,
			r.Method,
		).Observe(duration)
	})
}

// corsMiddleware handles CORS
func (g *InboundGateway) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*") // Configure as needed
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, X-Trace-Id")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requestIDMiddleware adds request ID
func (g *InboundGateway) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		ctx := context.WithValue(r.Context(), contextKeyRequestID, requestID)
		w.Header().Set("X-Request-ID", requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// timeoutMiddleware enforces request timeout
func (g *InboundGateway) timeoutMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), g.config.RequestTimeout)
		defer cancel()

		done := make(chan struct{})
		go func() {
			next.ServeHTTP(w, r.WithContext(ctx))
			close(done)
		}()

		select {
		case <-done:
			// Request completed successfully
		case <-ctx.Done():
			// Timeout occurred
			g.logger.Warn("Request timeout",
				zap.String("path", r.URL.Path),
				zap.String("trace_id", getTraceID(r.Context())),
			)
			http.Error(w, "Request Timeout", http.StatusRequestTimeout)
		}
	})
}

// authenticationMiddleware handles authentication
func (g *InboundGateway) authenticationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		integrationType := mux.Vars(r)["integrationType"]
		
		// Get integration config
		integrationConfig, exists := g.config.IntegrationConfigs[integrationType]
		if !exists || !integrationConfig.Enabled {
			http.Error(w, "Integration not found or disabled", http.StatusNotFound)
			return
		}

		// Check if authentication is required
		if !integrationConfig.AuthRequirements.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		// Perform authentication
		claims, err := g.authenticator.Authenticate(r, integrationConfig.AuthRequirements)
		if err != nil {
			g.logger.Warn("Authentication failed",
				zap.Error(err),
				zap.String("integration", integrationType),
				zap.String("trace_id", getTraceID(r.Context())),
			)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Add claims to context
		ctx := context.WithValue(r.Context(), contextKeyAuthClaims, claims)
		ctx = context.WithValue(ctx, contextKeyClientID, claims.ClientID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// rateLimitMiddleware enforces rate limiting
func (g *InboundGateway) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		integrationType := mux.Vars(r)["integrationType"]
		clientID := getClientID(r.Context())

		// Check rate limit
		allowed, err := g.rateLimiter.Allow(r.Context(), integrationType, clientID)
		if err != nil {
			g.logger.Error("Rate limiter error",
				zap.Error(err),
				zap.String("integration", integrationType),
			)
			// Continue on error (fail open)
		}

		if !allowed {
			g.logger.Warn("Rate limit exceeded",
				zap.String("integration", integrationType),
				zap.String("client_id", clientID),
			)

			w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%.0f", g.config.RateLimitConfig.RequestsPerSecond))
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Second).Unix()))
			
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// circuitBreakerMiddleware implements circuit breaker pattern
func (g *InboundGateway) circuitBreakerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		integrationType := mux.Vars(r)["integrationType"]

		// Check circuit breaker
		err := g.circuitBreaker.Call(integrationType, func() error {
			next.ServeHTTP(w, r)
			
			// Check if response indicates failure
			if wrapped, ok := w.(*responseWriter); ok && wrapped.statusCode >= 500 {
				return fmt.Errorf("server error: %d", wrapped.statusCode)
			}
			return nil
		})

		if err != nil {
			if strings.Contains(err.Error(), "circuit breaker is open") {
				g.logger.Warn("Circuit breaker open",
					zap.String("integration", integrationType),
				)
				http.Error(w, "Service temporarily unavailable", http.StatusServiceUnavailable)
				return
			}

			// Other errors are already handled by the wrapped handler
			if !strings.Contains(err.Error(), "server error") {
				g.logger.Error("Circuit breaker error",
					zap.Error(err),
					zap.String("integration", integrationType),
				)
			}
		}
	})
}

// webhookAuthMiddleware handles webhook authentication
func (g *InboundGateway) webhookAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider := mux.Vars(r)["provider"]

		// Verify webhook signature based on provider
		if err := g.authenticator.VerifyWebhookSignature(r, provider); err != nil {
			g.logger.Warn("Webhook verification failed",
				zap.Error(err),
				zap.String("provider", provider),
			)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// batchRateLimitMiddleware applies different rate limits for batch operations
func (g *InboundGateway) batchRateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID := getClientID(r.Context())

		// Apply batch-specific rate limits
		allowed, err := g.rateLimiter.AllowBatch(r.Context(), clientID)
		if err != nil || !allowed {
			http.Error(w, "Batch rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// adminAuthMiddleware handles admin authentication
func (g *InboundGateway) adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify admin credentials
		if !g.authenticator.IsAdmin(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleIntegration handles main integration requests
func (g *InboundGateway) handleIntegration(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vars := mux.Vars(r)
	integrationType := vars["integrationType"]

	// Get integration config
	integrationConfig, exists := g.config.IntegrationConfigs[integrationType]
	if !exists {
		g.respondWithError(w, http.StatusNotFound, "Integration not found")
		return
	}

	// Parse request
	req, err := g.parseRequest(r, integrationType)
	if err != nil {
		g.logger.Error("Failed to parse request",
			zap.Error(err),
			zap.String("integration", integrationType),
		)
		g.respondWithError(w, http.StatusBadRequest, "Invalid request format")
		return
	}

	// Validate request
	if err := g.validator.Validate(req, integrationConfig.ValidationRules); err != nil {
		g.logger.Warn("Validation failed",
			zap.Error(err),
			zap.String("integration", integrationType),
		)
		g.respondWithValidationError(w, err)
		return
	}

	// Transform request if needed
	if integrationConfig.TransformationRules != nil && integrationConfig.TransformationRules.Enabled {
		transformedReq, err := g.transformRequest(req, integrationConfig.TransformationRules)
		if err != nil {
			g.logger.Error("Transformation failed",
				zap.Error(err),
				zap.String("integration", integrationType),
			)
			g.respondWithError(w, http.StatusInternalServerError, "Transformation failed")
			return
		}
		req = transformedReq
	}

	// Route message
	topics, err := g.router.Route(req, integrationConfig.RoutingRules)
	if err != nil {
		g.logger.Error("Routing failed",
			zap.Error(err),
			zap.String("integration", integrationType),
		)
		g.respondWithError(w, http.StatusInternalServerError, "Routing failed")
		return
	}

	// Publish to Pub/Sub
	messageIDs := make([]string, 0)
	for _, topic := range topics {
		messageID, err := g.publisher.Publish(ctx, topic, req)
		if err != nil {
			g.logger.Error("Failed to publish message",
				zap.Error(err),
				zap.String("topic", topic),
				zap.String("integration", integrationType),
			)
			// Continue with other topics
			continue
		}
		messageIDs = append(messageIDs, messageID)
	}

	if len(messageIDs) == 0 {
		g.respondWithError(w, http.StatusInternalServerError, "Failed to process request")
		return
	}

	// Send response
	response := Response{
		ID:      req.ID,
		Status:  "success",
		Message: "Request processed successfully",
		Data: map[string]interface{}{
			"message_ids": messageIDs,
			"topics":      topics,
		},
		Timestamp: time.Now(),
		TraceID:   getTraceID(ctx),
	}

	g.respondWithJSON(w, http.StatusAccepted, response)
}

// handleWebhook handles webhook requests
func (g *InboundGateway) handleWebhook(w http.ResponseWriter, r *http.Request) {
	provider := mux.Vars(r)["provider"]
	
	// Parse webhook payload
	body, err := io.ReadAll(io.LimitReader(r.Body, g.config.MaxRequestSize))
	if err != nil {
		g.logger.Error("Failed to read webhook body",
			zap.Error(err),
			zap.String("provider", provider),
		)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Create webhook request
	req := &Request{
		ID:              uuid.New().String(),
		IntegrationType: "webhook_" + provider,
		Method:          r.Method,
		Path:            r.URL.Path,
		Headers:         r.Header,
		Body:            json.RawMessage(body),
		Timestamp:       time.Now(),
		TraceID:         getTraceID(r.Context()),
		Metadata: map[string]interface{}{
			"provider": provider,
			"webhook":  true,
		},
	}

	// Publish to webhook topic
	topic := fmt.Sprintf("webhooks.%s", provider)
	messageID, err := g.publisher.Publish(r.Context(), topic, req)
	if err != nil {
		g.logger.Error("Failed to publish webhook",
			zap.Error(err),
			zap.String("provider", provider),
		)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Acknowledge webhook receipt
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":     "received",
		"message_id": messageID,
	})
}

// Helper functions

func (g *InboundGateway) parseRequest(r *http.Request, integrationType string) (*Request, error) {
	req := &Request{
		ID:              uuid.New().String(),
		IntegrationType: integrationType,
		Method:          r.Method,
		Path:            r.URL.Path,
		Headers:         r.Header,
		QueryParams:     r.URL.Query(),
		ClientID:        getClientID(r.Context()),
		Timestamp:       time.Now(),
		TraceID:         getTraceID(r.Context()),
		SpanID:          getSpanID(r.Context()),
		Metadata:        make(map[string]interface{}),
	}

	// Parse body if present
	if r.Body != nil && r.Method != "GET" && r.Method != "DELETE" {
		body, err := io.ReadAll(io.LimitReader(r.Body, g.config.MaxRequestSize))
		if err != nil {
			return nil, fmt.Errorf("failed to read body: %w", err)
		}
		if len(body) > 0 {
			req.Body = json.RawMessage(body)
		}
	}

	return req, nil
}

func (g *InboundGateway) transformRequest(req *Request, rules *TransformationRules) (*Request, error) {
	// Apply transformations
	// This would implement the actual transformation logic
	return req, nil
}

func (g *InboundGateway) respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		g.logger.Error("Failed to encode response", zap.Error(err))
	}
}

func (g *InboundGateway) respondWithError(w http.ResponseWriter, code int, message string) {
	response := Response{
		ID:        uuid.New().String(),
		Status:    "error",
		Message:   message,
		Timestamp: time.Now(),
	}
	g.respondWithJSON(w, code, response)
}

func (g *InboundGateway) respondWithValidationError(w http.ResponseWriter, err error) {
	response := Response{
		ID:        uuid.New().String(),
		Status:    "error",
		Message:   "Validation failed",
		Errors:    []Error{{Code: "VALIDATION_ERROR", Message: err.Error()}},
		Timestamp: time.Now(),
	}
	g.respondWithJSON(w, http.StatusBadRequest, response)
}

// Context helper functions
func getTraceID(ctx context.Context) string {
	if v := ctx.Value(contextKeyTraceID); v != nil {
		return v.(string)
	}
	return ""
}

func getSpanID(ctx context.Context) string {
	if v := ctx.Value(contextKeySpanID); v != nil {
		return v.(string)
	}
	return ""
}

func getClientID(ctx context.Context) string {
	if v := ctx.Value(contextKeyClientID); v != nil {
		return v.(string)
	}
	return ""
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.written {
		rw.statusCode = code
		rw.ResponseWriter.WriteHeader(code)
		rw.written = true
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// handleHealth returns the health status of the gateway
func (g *InboundGateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().UTC(),
		"service":   "inbound-gateway",
		"version":   "1.0.0",
	}

	g.respondWithJSON(w, http.StatusOK, health)
}

// handleReady returns the readiness status for Kubernetes probes
func (g *InboundGateway) handleReady(w http.ResponseWriter, r *http.Request) {
	// Check if all critical components are ready
	isReady := true
	checks := make(map[string]bool)

	// Check publisher health
	if g.publisher != nil {
		checks["publisher"] = g.publisher.IsHealthy()
		isReady = isReady && checks["publisher"]
	} else {
		checks["publisher"] = false
		isReady = false
	}

	// Check if authenticator is initialized
	checks["authenticator"] = g.authenticator != nil
	isReady = isReady && checks["authenticator"]

	// Check if validator is initialized
	checks["validator"] = g.validator != nil
	isReady = isReady && checks["validator"]

	// Check if router is initialized
	checks["router"] = g.router != nil
	isReady = isReady && checks["router"]

	status := map[string]interface{}{
		"ready":     isReady,
		"timestamp": time.Now().UTC(),
		"checks":    checks,
	}

	if isReady {
		g.respondWithJSON(w, http.StatusOK, status)
	} else {
		g.respondWithJSON(w, http.StatusServiceUnavailable, status)
	}
}

// graphqlHandler returns an HTTP handler for GraphQL requests
func (g *InboundGateway) graphqlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse GraphQL request
		var graphqlReq struct {
			Query         string                 `json:"query"`
			OperationName string                 `json:"operationName,omitempty"`
			Variables     map[string]interface{} `json:"variables,omitempty"`
		}

		if err := json.NewDecoder(r.Body).Decode(&graphqlReq); err != nil {
			g.logger.Error("Failed to parse GraphQL request", zap.Error(err))
			g.respondWithError(w, http.StatusBadRequest, "Invalid GraphQL request")
			return
		}

		// Convert GraphQL request to standard Request format
		body, _ := json.Marshal(graphqlReq)
		req := &Request{
			ID:              uuid.New().String(),
			IntegrationType: "graphql",
			Method:          r.Method,
			Path:            r.URL.Path,
			Headers:         r.Header,
			Body:            json.RawMessage(body),
			ClientID:        getClientID(r.Context()),
			Timestamp:       time.Now(),
			TraceID:         getTraceID(r.Context()),
			SpanID:          getSpanID(r.Context()),
			Metadata: map[string]interface{}{
				"operation_name": graphqlReq.OperationName,
				"has_variables":  len(graphqlReq.Variables) > 0,
			},
		}

		// Get GraphQL integration config
		integrationConfig, exists := g.config.IntegrationConfigs["graphql"]
		if !exists || !integrationConfig.Enabled {
			g.respondWithError(w, http.StatusNotFound, "GraphQL integration not enabled")
			return
		}

		// Validate request
		if err := g.validator.Validate(req, integrationConfig.ValidationRules); err != nil {
			g.logger.Warn("GraphQL validation failed", zap.Error(err))
			g.respondWithValidationError(w, err)
			return
		}

		// Route to appropriate topic
		topics, err := g.router.Route(req, integrationConfig.RoutingRules)
		if err != nil {
			g.logger.Error("GraphQL routing failed", zap.Error(err))
			g.respondWithError(w, http.StatusInternalServerError, "Routing failed")
			return
		}

		// Publish to Pub/Sub
		messageIDs := make([]string, 0)
		for _, topic := range topics {
			messageID, err := g.publisher.Publish(r.Context(), topic, req)
			if err != nil {
				g.logger.Error("Failed to publish GraphQL message",
					zap.Error(err),
					zap.String("topic", topic),
				)
				continue
			}
			messageIDs = append(messageIDs, messageID)
		}

		if len(messageIDs) == 0 {
			g.respondWithError(w, http.StatusInternalServerError, "Failed to process GraphQL request")
			return
		}

		// Send response
		response := Response{
			ID:      req.ID,
			Status:  "success",
			Message: "GraphQL request processed successfully",
			Data: map[string]interface{}{
				"message_ids": messageIDs,
				"topics":      topics,
			},
			Timestamp: time.Now(),
			TraceID:   getTraceID(r.Context()),
		}

		g.respondWithJSON(w, http.StatusAccepted, response)
	})
}

// handleBatchUpload handles batch file uploads
func (g *InboundGateway) handleBatchUpload(w http.ResponseWriter, r *http.Request) {
	// Parse multipart form
	if err := r.ParseMultipartForm(g.config.MaxRequestSize); err != nil {
		g.logger.Error("Failed to parse multipart form", zap.Error(err))
		g.respondWithError(w, http.StatusBadRequest, "Invalid multipart form")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		g.logger.Error("Failed to get file from form", zap.Error(err))
		g.respondWithError(w, http.StatusBadRequest, "File not found in request")
		return
	}
	defer file.Close()

	// Read file content
	content, err := io.ReadAll(io.LimitReader(file, g.config.MaxRequestSize))
	if err != nil {
		g.logger.Error("Failed to read file content", zap.Error(err))
		g.respondWithError(w, http.StatusBadRequest, "Failed to read file")
		return
	}

	// Create batch request
	batchID := uuid.New().String()
	req := &Request{
		ID:              batchID,
		IntegrationType: "batch",
		Method:          r.Method,
		Path:            r.URL.Path,
		Headers:         r.Header,
		Body:            json.RawMessage(content),
		ClientID:        getClientID(r.Context()),
		Timestamp:       time.Now(),
		TraceID:         getTraceID(r.Context()),
		SpanID:          getSpanID(r.Context()),
		Metadata: map[string]interface{}{
			"batch_id":    batchID,
			"filename":    header.Filename,
			"size":        len(content),
			"content_type": header.Header.Get("Content-Type"),
		},
	}

	// Publish to batch topic
	topic := "integrations.batch"
	messageID, err := g.publisher.Publish(r.Context(), topic, req)
	if err != nil {
		g.logger.Error("Failed to publish batch upload",
			zap.Error(err),
			zap.String("batch_id", batchID),
		)
		g.respondWithError(w, http.StatusInternalServerError, "Failed to process batch upload")
		return
	}

	// Send response
	response := Response{
		ID:      batchID,
		Status:  "accepted",
		Message: "Batch upload accepted for processing",
		Data: map[string]interface{}{
			"batch_id":   batchID,
			"message_id": messageID,
			"filename":   header.Filename,
			"size":       len(content),
		},
		Timestamp: time.Now(),
		TraceID:   getTraceID(r.Context()),
	}

	g.respondWithJSON(w, http.StatusAccepted, response)
}

// handleBatchStatus returns the status of a batch job
func (g *InboundGateway) handleBatchStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	batchID := vars["batchId"]

	if batchID == "" {
		g.respondWithError(w, http.StatusBadRequest, "Batch ID is required")
		return
	}

	// In a real implementation, this would query a database or cache
	// For now, return a placeholder response
	status := map[string]interface{}{
		"batch_id":   batchID,
		"status":     "processing",
		"created_at": time.Now().Add(-5 * time.Minute).UTC(),
		"updated_at": time.Now().UTC(),
		"progress": map[string]interface{}{
			"total":      100,
			"processed":  45,
			"failed":     2,
			"successful": 43,
		},
		"message": "Batch is being processed",
	}

	g.respondWithJSON(w, http.StatusOK, status)
}

// handleConfigReload reloads the gateway configuration
func (g *InboundGateway) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	g.logger.Info("Configuration reload requested",
		zap.String("requested_by", getClientID(r.Context())),
		zap.String("trace_id", getTraceID(r.Context())),
	)

	// Trigger config reload
	g.mu.Lock()
	defer g.mu.Unlock()

	// In a real implementation, this would reload from the config source
	// For now, trigger the background config reloader
	go g.reloadConfig()

	response := Response{
		ID:        uuid.New().String(),
		Status:    "success",
		Message:   "Configuration reload initiated",
		Timestamp: time.Now(),
		TraceID:   getTraceID(r.Context()),
	}

	g.respondWithJSON(w, http.StatusOK, response)
}

// handleCircuitBreakerReset resets the circuit breaker for a specific integration
func (g *InboundGateway) handleCircuitBreakerReset(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	integrationType := vars["integrationType"]

	if integrationType == "" {
		g.respondWithError(w, http.StatusBadRequest, "Integration type is required")
		return
	}

	g.logger.Info("Circuit breaker reset requested",
		zap.String("integration", integrationType),
		zap.String("requested_by", getClientID(r.Context())),
		zap.String("trace_id", getTraceID(r.Context())),
	)

	// Reset circuit breaker
	if err := g.circuitBreaker.Reset(integrationType); err != nil {
		g.logger.Error("Failed to reset circuit breaker",
			zap.Error(err),
			zap.String("integration", integrationType),
		)
		g.respondWithError(w, http.StatusInternalServerError, "Failed to reset circuit breaker")
		return
	}

	response := Response{
		ID:      uuid.New().String(),
		Status:  "success",
		Message: fmt.Sprintf("Circuit breaker reset for integration: %s", integrationType),
		Data: map[string]interface{}{
			"integration": integrationType,
			"state":       "closed",
		},
		Timestamp: time.Now(),
		TraceID:   getTraceID(r.Context()),
	}

	g.respondWithJSON(w, http.StatusOK, response)
}

// handleRateLimitUpdate updates rate limits dynamically
func (g *InboundGateway) handleRateLimitUpdate(w http.ResponseWriter, r *http.Request) {
	var updateReq struct {
		RequestsPerSecond float64 `json:"requests_per_second"`
		Burst             int     `json:"burst"`
	}

	if err := json.NewDecoder(r.Body).Decode(&updateReq); err != nil {
		g.logger.Error("Failed to parse rate limit update request", zap.Error(err))
		g.respondWithError(w, http.StatusBadRequest, "Invalid request format")
		return
	}

	if updateReq.RequestsPerSecond <= 0 {
		g.respondWithError(w, http.StatusBadRequest, "Requests per second must be positive")
		return
	}

	g.logger.Info("Rate limit update requested",
		zap.Float64("requests_per_second", updateReq.RequestsPerSecond),
		zap.Int("burst", updateReq.Burst),
		zap.String("requested_by", getClientID(r.Context())),
	)

	// Update rate limiter (applies globally to all integrations)
	g.rateLimiter.UpdateLimits(updateReq.RequestsPerSecond, updateReq.Burst)

	response := Response{
		ID:      uuid.New().String(),
		Status:  "success",
		Message: "Rate limits updated successfully for all integrations",
		Data: map[string]interface{}{
			"requests_per_second": updateReq.RequestsPerSecond,
			"burst":               updateReq.Burst,
		},
		Timestamp: time.Now(),
		TraceID:   getTraceID(r.Context()),
	}

	g.respondWithJSON(w, http.StatusOK, response)
}
