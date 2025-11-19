package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/your-org/inbound-gateway/internal/gateway"
	"go.uber.org/zap"
)

func TestInboundGateway_HandleIntegration(t *testing.T) {
	// Create test configuration
	config := &gateway.Config{
		Port:            "8080",
		ProjectID:       "test-project",
		MaxRequestSize:  1024 * 1024,
		RequestTimeout:  30 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		EnableMetrics:   true,
		EnableTracing:   true,
		
		IntegrationConfigs: map[string]*gateway.IntegrationConfig{
			"test": {
				Name:    "test",
				Type:    "rest",
				Enabled: true,
				ValidationRules: gateway.ValidationRules{
					RequiredFields: []string{"name"},
					MaxPayloadSize: 1024,
					ContentTypes:   []string{"application/json"},
				},
				RoutingRules: gateway.RoutingRules{
					DefaultTopic: "test-topic",
				},
				AuthRequirements: gateway.AuthRequirements{
					Enabled: false,
				},
			},
		},
	}

	// Create gateway
	gw, err := gateway.NewInboundGateway(config)
	if err != nil {
		t.Fatalf("Failed to create gateway: %v", err)
	}

	// Test cases
	tests := []struct {
		name           string
		method         string
		path           string
		body           interface{}
		expectedStatus int
	}{
		{
			name:   "Valid request",
			method: "POST",
			path:   "/v1/integrations/test",
			body: map[string]interface{}{
				"name": "test-item",
				"value": 123,
			},
			expectedStatus: http.StatusAccepted,
		},
		{
			name:   "Missing required field",
			method: "POST",
			path:   "/v1/integrations/test",
			body: map[string]interface{}{
				"value": 123,
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "Invalid integration",
			method:         "POST",
			path:           "/v1/integrations/invalid",
			body:           map[string]interface{}{},
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Prepare request
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")

			// Create response recorder
			rr := httptest.NewRecorder()

			// Setup routes
			router := gw.SetupRoutes()

			// Serve request
			router.ServeHTTP(rr, req)

			// Check status code
			if rr.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, rr.Code)
				t.Logf("Response body: %s", rr.Body.String())
			}
		})
	}
}

func TestRateLimiter(t *testing.T) {
	config := gateway.RateLimitConfig{
		Enabled:           true,
		RequestsPerSecond: 10,
		Burst:             20,
		PerClient:         true,
		PerIntegration:    true,
	}

	logger, _ := zap.NewDevelopment()
	
	rateLimiter, err := gateway.NewRateLimiterManager(config, logger)
	if err != nil {
		t.Fatalf("Failed to create rate limiter: %v", err)
	}

	// Test rate limiting
	clientID := "test-client"
	integrationType := "test-integration"
	
	// Should allow burst
	for i := 0; i < 20; i++ {
		allowed, err := rateLimiter.Allow(context.Background(), integrationType, clientID)
		if err != nil {
			t.Errorf("Rate limiter error: %v", err)
		}
		if !allowed {
			t.Errorf("Request %d should be allowed within burst limit", i+1)
		}
	}

	// Should block after burst
	allowed, _ := rateLimiter.Allow(context.Background(), integrationType, clientID)
	if allowed {
		t.Error("Request should be blocked after burst limit")
	}
}

func TestCircuitBreaker(t *testing.T) {
	config := gateway.CircuitBreakerConfig{
		Enabled:             true,
		FailureThreshold:    0.5,
		SuccessThreshold:    0.8,
		Timeout:             1 * time.Second,
		HalfOpenMaxRequests: 3,
		ObservationWindow:   5 * time.Second,
	}

	logger, _ := zap.NewDevelopment()
	
	cb, err := gateway.NewCircuitBreakerManager(config, logger)
	if err != nil {
		t.Fatalf("Failed to create circuit breaker: %v", err)
	}

	integration := "test-integration"

	// Simulate failures to open circuit
	for i := 0; i < 10; i++ {
		err := cb.Call(integration, func() error {
			if i%2 == 0 {
				return fmt.Errorf("simulated error")
			}
			return nil
		})
		
		// After enough failures, circuit should open
		if i > 5 && err != nil && strings.Contains(err.Error(), "circuit breaker is open") {
			// Circuit opened as expected
			break
		}
	}

	// Check state
	state := cb.GetState(integration)
	if state != "open" && state != "half-open" {
		t.Errorf("Expected circuit to be open or half-open, got %s", state)
	}
}

func TestAuthenticator(t *testing.T) {
	config := &gateway.AuthConfig{
		JWTConfig: &gateway.JWTConfig{
			Secret:    "test-secret",
			Issuer:    "test-issuer",
			Audience:  []string{"test-audience"},
			ExpiresIn: 3600,
		},
		APIKeyConfig: &gateway.APIKeyConfig{
			HeaderName: "X-API-Key",
			Keys: map[string]*gateway.APIKeyInfo{
				"test-key": {
					Key:      "test-key",
					ClientID: "test-client",
					Scopes:   []string{"read"},
					Roles:    []string{"user"},
					Enabled:  true,
				},
			},
		},
	}

	logger, _ := zap.NewDevelopment()
	
	auth, err := gateway.NewAuthenticator(config, logger)
	if err != nil {
		t.Fatalf("Failed to create authenticator: %v", err)
	}

	// Test API Key authentication
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "test-key")

	requirements := gateway.AuthRequirements{
		Enabled: true,
		Methods: []string{"api_key"},
	}

	claims, err := auth.Authenticate(req, requirements)
	if err != nil {
		t.Errorf("Authentication failed: %v", err)
	}

	if claims.ClientID != "test-client" {
		t.Errorf("Expected client ID 'test-client', got '%s'", claims.ClientID)
	}
}

func BenchmarkHandleIntegration(b *testing.B) {
	// Setup gateway
	config := createTestConfig()
	gw, _ := gateway.NewInboundGateway(config)
	router := gw.SetupRoutes()

	// Prepare request
	body := []byte(`{"name": "test", "value": 123}`)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("POST", "/v1/integrations/test", bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
		}
	})
}

func createTestConfig() *gateway.Config {
	return &gateway.Config{
		Port:            "8080",
		ProjectID:       "test-project",
		MaxRequestSize:  1024 * 1024,
		RequestTimeout:  30 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		EnableMetrics:   false,
		EnableTracing:   false,
		
		IntegrationConfigs: map[string]*gateway.IntegrationConfig{
			"test": {
				Name:    "test",
				Type:    "rest",
				Enabled: true,
				ValidationRules: gateway.ValidationRules{
					MaxPayloadSize: 1024,
					ContentTypes:   []string{"application/json"},
				},
				RoutingRules: gateway.RoutingRules{
					DefaultTopic: "test-topic",
				},
				AuthRequirements: gateway.AuthRequirements{
					Enabled: false,
				},
			},
		},
	}
}
