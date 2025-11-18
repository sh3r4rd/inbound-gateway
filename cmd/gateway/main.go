package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	
	"github.com/your-org/inbound-gateway/internal/gateway"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "config/config.yaml", "Path to configuration file")
	flag.Parse()

	// Initialize logger
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	// Load configuration
	config, err := loadConfig(configPath)
	if err != nil {
		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	// Create gateway
	gw, err := gateway.NewInboundGateway(config)
	if err != nil {
		logger.Fatal("Failed to create gateway", zap.Error(err))
	}

	// Start gateway
	ctx := context.Background()
	if err := gw.Start(ctx); err != nil {
		logger.Fatal("Failed to start gateway", zap.Error(err))
	}

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	logger.Info("Shutting down gateway...")

	// Graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(ctx, config.ShutdownTimeout)
	defer cancel()

	if err := gw.Stop(shutdownCtx); err != nil {
		logger.Error("Failed to stop gateway gracefully", zap.Error(err))
	} else {
		logger.Info("Gateway stopped successfully")
	}
}

func loadConfig(path string) (*gateway.Config, error) {
	viper.SetConfigFile(path)
	viper.SetEnvPrefix("GATEWAY")
	viper.AutomaticEnv()

	// Set defaults
	setDefaults()

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		// Config file not found; use defaults and env vars
		fmt.Println("Config file not found, using defaults and environment variables")
	}

	var config gateway.Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Load integration configs
	if err := loadIntegrationConfigs(&config); err != nil {
		return nil, fmt.Errorf("failed to load integration configs: %w", err)
	}

	return &config, nil
}

func setDefaults() {
	viper.SetDefault("port", "8080")
	viper.SetDefault("project_id", os.Getenv("GCP_PROJECT_ID"))
	viper.SetDefault("max_request_size", 10*1024*1024) // 10MB
	viper.SetDefault("request_timeout", "30s")
	viper.SetDefault("shutdown_timeout", "30s")
	viper.SetDefault("enable_metrics", true)
	viper.SetDefault("enable_tracing", true)

	// Rate limiting defaults
	viper.SetDefault("rate_limit.enabled", true)
	viper.SetDefault("rate_limit.requests_per_second", 100.0)
	viper.SetDefault("rate_limit.burst", 200)
	viper.SetDefault("rate_limit.per_client", true)
	viper.SetDefault("rate_limit.per_integration", true)

	// Circuit breaker defaults
	viper.SetDefault("circuit_breaker.enabled", true)
	viper.SetDefault("circuit_breaker.failure_threshold", 0.5)
	viper.SetDefault("circuit_breaker.success_threshold", 0.8)
	viper.SetDefault("circuit_breaker.timeout", "60s")
	viper.SetDefault("circuit_breaker.half_open_max_requests", 3)
	viper.SetDefault("circuit_breaker.observation_window", "60s")

	// PubSub defaults
	viper.SetDefault("pubsub.project_id", os.Getenv("GCP_PROJECT_ID"))
	viper.SetDefault("pubsub.topic_prefix", "integrations")
	viper.SetDefault("pubsub.enable_message_ordering", false)
	viper.SetDefault("pubsub.enable_retries", true)
	viper.SetDefault("pubsub.publish_timeout", "10s")
	viper.SetDefault("pubsub.batching_enabled", true)

	// Retry defaults
	viper.SetDefault("pubsub.retry_config.max_retries", 3)
	viper.SetDefault("pubsub.retry_config.initial_delay", "1s")
	viper.SetDefault("pubsub.retry_config.max_delay", "10s")
	viper.SetDefault("pubsub.retry_config.multiplier", 2.0)
	viper.SetDefault("pubsub.retry_config.randomization_factor", 0.1)

	// Batching defaults
	viper.SetDefault("pubsub.batching_config.max_messages", 100)
	viper.SetDefault("pubsub.batching_config.max_bytes", 1000000)
	viper.SetDefault("pubsub.batching_config.max_latency", "100ms")
}

func loadIntegrationConfigs(config *gateway.Config) error {
	// Load integration configurations from separate files or database
	// For now, we'll create some example integrations
	
	config.IntegrationConfigs = make(map[string]*gateway.IntegrationConfig)

	// Example: Uber API integration
	uberIntegration := &gateway.IntegrationConfig{
		Name:    "uber",
		Type:    "rest",
		Enabled: true,
		ValidationRules: gateway.ValidationRules{
			RequiredFields: []string{"pickup_location", "destination"},
			MaxPayloadSize: 1024 * 1024, // 1MB
			ContentTypes:   []string{"application/json"},
		},
		RoutingRules: gateway.RoutingRules{
			DefaultTopic: "integrations.uber",
		},
		AuthRequirements: gateway.AuthRequirements{
			Enabled:  true,
			Methods:  []string{"oauth2", "api_key"},
			Required: true,
		},
	}
	config.IntegrationConfigs["uber"] = uberIntegration

	// Example: EPIC EHR integration
	epicIntegration := &gateway.IntegrationConfig{
		Name:    "epic",
		Type:    "rest",
		Enabled: true,
		ValidationRules: gateway.ValidationRules{
			RequiredFields: []string{"patient_id", "request_type"},
			MaxPayloadSize: 5 * 1024 * 1024, // 5MB
			ContentTypes:   []string{"application/json", "application/xml"},
		},
		RoutingRules: gateway.RoutingRules{
			DefaultTopic: "integrations.epic",
			ConditionalRoutes: []gateway.ConditionalRoute{
				{
					Condition: `body.request_type == "patient_data"`,
					Topic:     "integrations.epic.patient",
					Priority:  1,
				},
				{
					Condition: `body.request_type == "appointment"`,
					Topic:     "integrations.epic.appointment",
					Priority:  2,
				},
			},
		},
		AuthRequirements: gateway.AuthRequirements{
			Enabled:       true,
			Methods:       []string{"oauth2", "jwt"},
			Required:      true,
			AllowedScopes: []string{"patient.read", "patient.write"},
		},
	}
	config.IntegrationConfigs["epic"] = epicIntegration

	// Example: Webhook integration
	webhookIntegration := &gateway.IntegrationConfig{
		Name:    "webhook",
		Type:    "webhook",
		Enabled: true,
		ValidationRules: gateway.ValidationRules{
			MaxPayloadSize: 10 * 1024 * 1024, // 10MB
			ContentTypes:   []string{"application/json"},
		},
		RoutingRules: gateway.RoutingRules{
			DefaultTopic: "integrations.webhooks",
		},
		AuthRequirements: gateway.AuthRequirements{
			Enabled:  true,
			Methods:  []string{"webhook_signature"},
			Required: true,
		},
	}
	config.IntegrationConfigs["webhook"] = webhookIntegration

	return nil
}
