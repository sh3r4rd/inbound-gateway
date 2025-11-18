package gateway

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
	"go.uber.org/zap"
)

// MessageRouter handles message routing logic
type MessageRouter struct {
	config *Config
	logger *zap.Logger
	celEnv *cel.Env
}

// NewMessageRouter creates a new message router
func NewMessageRouter(config *Config, logger *zap.Logger) (*MessageRouter, error) {
	// Initialize CEL environment for conditional routing
	env, err := cel.NewEnv(
		cel.Declarations(
			decls.NewVar("body", decls.NewMapType(decls.String, decls.Dyn)),
			decls.NewVar("headers", decls.NewMapType(decls.String, decls.String)),
			decls.NewVar("method", decls.String),
			decls.NewVar("path", decls.String),
			decls.NewVar("client_id", decls.String),
			decls.NewVar("integration_type", decls.String),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize CEL environment: %w", err)
	}

	return &MessageRouter{
		config: config,
		logger: logger,
		celEnv: env,
	}, nil
}

// Route determines which topics a message should be routed to
func (r *MessageRouter) Route(req *Request, rules RoutingRules) ([]string, error) {
	topics := make([]string, 0)

	// Check conditional routes first
	if len(rules.ConditionalRoutes) > 0 {
		conditionalTopic, err := r.evaluateConditionalRoutes(req, rules.ConditionalRoutes)
		if err != nil {
			r.logger.Error("Failed to evaluate conditional routes", zap.Error(err))
		} else if conditionalTopic != "" {
			topics = append(topics, conditionalTopic)
			return topics, nil // Return early if conditional route matches
		}
	}

	// Check header-based routing
	if len(rules.HeaderBasedRouting) > 0 {
		headerTopic := r.evaluateHeaderRouting(req, rules.HeaderBasedRouting)
		if headerTopic != "" {
			topics = append(topics, headerTopic)
			return topics, nil
		}
	}

	// Use default topic
	if rules.DefaultTopic != "" {
		topics = append(topics, rules.DefaultTopic)
	} else {
		// Fallback to integration-based topic
		topics = append(topics, fmt.Sprintf("integrations.%s", req.IntegrationType))
	}

	// Apply partitioning if configured
	if rules.Partitioning != nil && rules.Partitioning.Enabled {
		topics = r.applyPartitioning(topics, req, rules.Partitioning)
	}

	return topics, nil
}

// evaluateConditionalRoutes evaluates conditional routing rules
func (r *MessageRouter) evaluateConditionalRoutes(req *Request, routes []ConditionalRoute) (string, error) {
	// Sort routes by priority (lower number = higher priority)
	// In production, you'd want to sort this once during initialization
	sortedRoutes := make([]ConditionalRoute, len(routes))
	copy(sortedRoutes, routes)
	// Simple bubble sort for demonstration
	for i := 0; i < len(sortedRoutes)-1; i++ {
		for j := 0; j < len(sortedRoutes)-i-1; j++ {
			if sortedRoutes[j].Priority > sortedRoutes[j+1].Priority {
				sortedRoutes[j], sortedRoutes[j+1] = sortedRoutes[j+1], sortedRoutes[j]
			}
		}
	}

	// Prepare request data for CEL evaluation
	var bodyData map[string]interface{}
	if len(req.Body) > 0 {
		// Parse body for evaluation
		// Note: In production, you'd want to handle this more robustly
		bodyData = make(map[string]interface{})
	}

	evalData := map[string]interface{}{
		"body":             bodyData,
		"headers":          req.Headers,
		"method":           req.Method,
		"path":             req.Path,
		"client_id":        req.ClientID,
		"integration_type": req.IntegrationType,
	}

	// Evaluate each route condition
	for _, route := range sortedRoutes {
		matches, err := r.evaluateCondition(route.Condition, evalData)
		if err != nil {
			r.logger.Warn("Failed to evaluate routing condition",
				zap.String("condition", route.Condition),
				zap.Error(err),
			)
			continue
		}

		if matches {
			r.logger.Debug("Conditional route matched",
				zap.String("condition", route.Condition),
				zap.String("topic", route.Topic),
			)
			return route.Topic, nil
		}
	}

	return "", nil
}

// evaluateCondition evaluates a CEL condition
func (r *MessageRouter) evaluateCondition(condition string, data map[string]interface{}) (bool, error) {
	// Parse and compile the CEL expression
	ast, issues := r.celEnv.Compile(condition)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("failed to compile condition: %w", issues.Err())
	}

	// Create the CEL program
	prg, err := r.celEnv.Program(ast)
	if err != nil {
		return false, fmt.Errorf("failed to create program: %w", err)
	}

	// Evaluate the expression
	out, _, err := prg.Eval(data)
	if err != nil {
		return false, fmt.Errorf("failed to evaluate condition: %w", err)
	}

	// Check if the result is a boolean
	result, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("condition did not return a boolean")
	}

	return result, nil
}

// evaluateHeaderRouting evaluates header-based routing
func (r *MessageRouter) evaluateHeaderRouting(req *Request, routing map[string]string) string {
	for header, topic := range routing {
		if values, exists := req.Headers[header]; exists && len(values) > 0 {
			r.logger.Debug("Header-based route matched",
				zap.String("header", header),
				zap.String("value", values[0]),
				zap.String("topic", topic),
			)
			return topic
		}
	}
	return ""
}

// applyPartitioning applies partitioning to topics
func (r *MessageRouter) applyPartitioning(topics []string, req *Request, config *PartitioningConfig) []string {
	if !config.Enabled || config.NumPartitions <= 1 {
		return topics
	}

	// Extract partition key
	partitionKey := r.extractPartitionKey(req, config.PartitionKey)
	if partitionKey == "" {
		r.logger.Warn("Failed to extract partition key, using default routing")
		return topics
	}

	// Calculate partition number
	partition := r.calculatePartition(partitionKey, config.NumPartitions)

	// Modify topics to include partition
	partitionedTopics := make([]string, len(topics))
	for i, topic := range topics {
		partitionedTopics[i] = fmt.Sprintf("%s.partition_%d", topic, partition)
	}

	r.logger.Debug("Applied partitioning",
		zap.String("partition_key", partitionKey),
		zap.Int("partition", partition),
		zap.Strings("topics", partitionedTopics),
	)

	return partitionedTopics
}

// extractPartitionKey extracts the partition key from the request
func (r *MessageRouter) extractPartitionKey(req *Request, keyPath string) string {
	if keyPath == "" {
		// Default to client ID
		return req.ClientID
	}

	// Support different key paths
	switch {
	case keyPath == "client_id":
		return req.ClientID
	case keyPath == "integration_type":
		return req.IntegrationType
	case strings.HasPrefix(keyPath, "header."):
		headerName := strings.TrimPrefix(keyPath, "header.")
		if values, exists := req.Headers[headerName]; exists && len(values) > 0 {
			return values[0]
		}
	case strings.HasPrefix(keyPath, "body."):
		// Extract from body (would need JSON parsing)
		// Simplified for demonstration
		return ""
	}

	return ""
}

// calculatePartition calculates the partition number for a key
func (r *MessageRouter) calculatePartition(key string, numPartitions int) int {
	// Simple hash-based partitioning
	hash := 0
	for _, char := range key {
		hash = (hash * 31) + int(char)
	}

	// Ensure positive partition number
	if hash < 0 {
		hash = -hash
	}

	return hash % numPartitions
}

// RouteToDeadLetter routes a message to the dead letter queue
func (r *MessageRouter) RouteToDeadLetter(req *Request, reason string) (string, error) {
	dlqTopic := fmt.Sprintf("integrations.dlq.%s", req.IntegrationType)
	
	r.logger.Info("Routing message to dead letter queue",
		zap.String("request_id", req.ID),
		zap.String("topic", dlqTopic),
		zap.String("reason", reason),
	)

	return dlqTopic, nil
}

// GetRoutingMetrics returns routing metrics
func (r *MessageRouter) GetRoutingMetrics() map[string]interface{} {
	// In production, you'd track actual routing metrics
	return map[string]interface{}{
		"total_routed":      0,
		"conditional_hits":  0,
		"header_hits":       0,
		"default_hits":      0,
		"partitioned_count": 0,
		"dlq_count":         0,
	}
}
