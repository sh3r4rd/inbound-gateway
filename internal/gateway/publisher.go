package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/api/option"
)

// PubSubPublisher handles publishing messages to Google Cloud Pub/Sub
type PubSubPublisher struct {
	client       *pubsub.Client
	config       PubSubConfig
	topics       map[string]*pubsub.Topic
	logger       *zap.Logger
	mu           sync.RWMutex
	healthy      bool
	publishStats *PublishStats
}

// PublishStats tracks publishing statistics
type PublishStats struct {
	mu               sync.RWMutex
	totalPublished   int64
	totalFailed      int64
	topicStats       map[string]*TopicStats
	lastPublishTime  time.Time
}

// TopicStats tracks statistics per topic
type TopicStats struct {
	Published  int64
	Failed     int64
	LastError  error
	LastPublish time.Time
}

// PublishResult represents the result of a publish operation
type PublishResult struct {
	MessageID string
	Topic     string
	Error     error
	Timestamp time.Time
}

// NewPubSubPublisher creates a new Pub/Sub publisher
func NewPubSubPublisher(config PubSubConfig, logger *zap.Logger) (*PubSubPublisher, error) {
	// Create Pub/Sub client
	ctx := context.Background()
	client, err := pubsub.NewClient(ctx, config.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("failed to create pubsub client: %w", err)
	}

	publisher := &PubSubPublisher{
		client:  client,
		config:  config,
		topics:  make(map[string]*pubsub.Topic),
		logger:  logger,
		healthy: true,
		publishStats: &PublishStats{
			topicStats: make(map[string]*TopicStats),
		},
	}

	// Configure client settings
	publisher.configureClient()

	// Initialize topics if needed
	if err := publisher.initializeTopics(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize topics: %w", err)
	}

	// Start health checker
	go publisher.healthCheck()

	return publisher, nil
}

// configureClient configures the Pub/Sub client settings
func (p *PubSubPublisher) configureClient() {
	// Configure default publish settings for all topics
	// These can be overridden per topic if needed
}

// initializeTopics initializes commonly used topics
func (p *PubSubPublisher) initializeTopics(ctx context.Context) error {
	// List of topics to pre-initialize
	commonTopics := []string{
		"integrations.inbound",
		"integrations.dlq",
		"integrations.batch",
	}

	for _, topicName := range commonTopics {
		fullTopicName := p.getFullTopicName(topicName)
		topic := p.client.Topic(fullTopicName)
		
		// Configure topic settings
		p.configureTopic(topic)
		
		// Check if topic exists
		exists, err := topic.Exists(ctx)
		if err != nil {
			return fmt.Errorf("failed to check topic existence %s: %w", fullTopicName, err)
		}

		if !exists {
			// Create topic if it doesn't exist
			_, err = p.client.CreateTopic(ctx, fullTopicName)
			if err != nil {
				p.logger.Warn("Failed to create topic",
					zap.String("topic", fullTopicName),
					zap.Error(err),
				)
				// Continue anyway - topic might be created elsewhere
			}
		}

		p.topics[topicName] = topic
	}

	return nil
}

// configureTopic configures topic-specific settings
func (p *PubSubPublisher) configureTopic(topic *pubsub.Topic) {
	// Configure batching settings
	if p.config.BatchingEnabled && p.config.BatchingConfig != nil {
		topic.PublishSettings.CountThreshold = p.config.BatchingConfig.MaxMessages
		topic.PublishSettings.ByteThreshold = p.config.BatchingConfig.MaxBytes
		topic.PublishSettings.DelayThreshold = p.config.BatchingConfig.MaxLatency
	}

	// Configure publish timeout
	topic.PublishSettings.Timeout = p.config.PublishTimeout

	// Configure message ordering if enabled
	if p.config.EnableMessageOrdering {
		topic.EnableMessageOrdering = true
	}

	// Configure retry settings
	if p.config.EnableRetries && p.config.RetryConfig != nil {
		// Note: Actual retry logic would be implemented in the publish method
		// as Pub/Sub client handles basic retries internally
	}
}

// Publish publishes a message to a topic
func (p *PubSubPublisher) Publish(ctx context.Context, topicName string, message interface{}) (string, error) {
	// Check health
	if !p.IsHealthy() {
		return "", fmt.Errorf("publisher is unhealthy")
	}

	// Get or create topic
	topic, err := p.getTopic(ctx, topicName)
	if err != nil {
		return "", fmt.Errorf("failed to get topic %s: %w", topicName, err)
	}

	// Serialize message
	data, err := p.serializeMessage(message)
	if err != nil {
		return "", fmt.Errorf("failed to serialize message: %w", err)
	}

	// Create Pub/Sub message
	pubsubMsg := &pubsub.Message{
		Data: data,
		Attributes: p.createMessageAttributes(message),
	}

	// Add ordering key if applicable
	if p.config.EnableMessageOrdering {
		if orderingKey := p.extractOrderingKey(message); orderingKey != "" {
			pubsubMsg.OrderingKey = orderingKey
		}
	}

	// Publish with timeout
	publishCtx, cancel := context.WithTimeout(ctx, p.config.PublishTimeout)
	defer cancel()

	// Publish the message
	result := topic.Publish(publishCtx, pubsubMsg)

	// Wait for the publish to complete
	messageID, err := result.Get(publishCtx)
	if err != nil {
		p.recordFailure(topicName, err)
		
		// Check if we should retry
		if p.shouldRetry(err) && p.config.EnableRetries {
			return p.retryPublish(ctx, topicName, message)
		}
		
		return "", fmt.Errorf("failed to publish message: %w", err)
	}

	// Record success
	p.recordSuccess(topicName)

	p.logger.Debug("Message published successfully",
		zap.String("topic", topicName),
		zap.String("message_id", messageID),
	)

	return messageID, nil
}

// PublishBatch publishes multiple messages to a topic
func (p *PubSubPublisher) PublishBatch(ctx context.Context, topicName string, messages []interface{}) ([]string, error) {
	if !p.IsHealthy() {
		return nil, fmt.Errorf("publisher is unhealthy")
	}

	topic, err := p.getTopic(ctx, topicName)
	if err != nil {
		return nil, fmt.Errorf("failed to get topic %s: %w", topicName, err)
	}

	var results []*pubsub.PublishResult
	messageIDs := make([]string, 0, len(messages))

	// Publish all messages
	for _, msg := range messages {
		data, err := p.serializeMessage(msg)
		if err != nil {
			p.logger.Error("Failed to serialize message in batch",
				zap.Error(err),
			)
			continue
		}

		pubsubMsg := &pubsub.Message{
			Data:       data,
			Attributes: p.createMessageAttributes(msg),
		}

		result := topic.Publish(ctx, pubsubMsg)
		results = append(results, result)
	}

	// Wait for all publishes to complete
	for i, result := range results {
		messageID, err := result.Get(ctx)
		if err != nil {
			p.logger.Error("Failed to publish message in batch",
				zap.Int("index", i),
				zap.Error(err),
			)
			p.recordFailure(topicName, err)
			continue
		}
		messageIDs = append(messageIDs, messageID)
		p.recordSuccess(topicName)
	}

	if len(messageIDs) == 0 {
		return nil, fmt.Errorf("all messages in batch failed to publish")
	}

	return messageIDs, nil
}

// PublishAsync publishes a message asynchronously
func (p *PubSubPublisher) PublishAsync(ctx context.Context, topicName string, message interface{}, callback func(string, error)) {
	go func() {
		messageID, err := p.Publish(ctx, topicName, message)
		if callback != nil {
			callback(messageID, err)
		}
	}()
}

// PublishToMultipleTopics publishes a message to multiple topics
func (p *PubSubPublisher) PublishToMultipleTopics(ctx context.Context, topicNames []string, message interface{}) (map[string]string, error) {
	results := make(map[string]string)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, topicName := range topicNames {
		wg.Add(1)
		go func(topic string) {
			defer wg.Done()
			
			messageID, err := p.Publish(ctx, topic, message)
			
			mu.Lock()
			defer mu.Unlock()
			
			if err != nil {
				p.logger.Error("Failed to publish to topic",
					zap.String("topic", topic),
					zap.Error(err),
				)
			} else {
				results[topic] = messageID
			}
		}(topicName)
	}

	wg.Wait()

	if len(results) == 0 {
		return nil, fmt.Errorf("failed to publish to any topic")
	}

	return results, nil
}

// PublishWithDeadLetter publishes a message with automatic dead letter handling
func (p *PubSubPublisher) PublishWithDeadLetter(ctx context.Context, topicName string, message interface{}) (string, error) {
	messageID, err := p.Publish(ctx, topicName, message)
	if err != nil {
		// Publish to dead letter queue
		dlqTopic := fmt.Sprintf("%s.dlq", topicName)
		dlqMessage := map[string]interface{}{
			"original_topic": topicName,
			"error":         err.Error(),
			"timestamp":     time.Now(),
			"message":       message,
		}
		
		dlqMessageID, dlqErr := p.Publish(ctx, dlqTopic, dlqMessage)
		if dlqErr != nil {
			p.logger.Error("Failed to publish to DLQ",
				zap.String("topic", dlqTopic),
				zap.Error(dlqErr),
			)
		} else {
			p.logger.Info("Message sent to DLQ",
				zap.String("dlq_topic", dlqTopic),
				zap.String("dlq_message_id", dlqMessageID),
			)
		}
		
		return "", err
	}
	
	return messageID, nil
}

// retryPublish retries publishing a message
func (p *PubSubPublisher) retryPublish(ctx context.Context, topicName string, message interface{}) (string, error) {
	if p.config.RetryConfig == nil {
		return "", fmt.Errorf("retry config not set")
	}

	retries := 0
	delay := p.config.RetryConfig.InitialDelay

	for retries < p.config.RetryConfig.MaxRetries {
		retries++
		
		// Wait before retrying
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}

		// Try to publish again
		messageID, err := p.Publish(ctx, topicName, message)
		if err == nil {
			p.logger.Info("Message published after retry",
				zap.String("topic", topicName),
				zap.Int("retries", retries),
				zap.String("message_id", messageID),
			)
			return messageID, nil
		}

		// Calculate next delay with exponential backoff
		delay = time.Duration(float64(delay) * p.config.RetryConfig.Multiplier)
		if delay > p.config.RetryConfig.MaxDelay {
			delay = p.config.RetryConfig.MaxDelay
		}

		// Add randomization
		if p.config.RetryConfig.RandomizationFactor > 0 {
			// Add jitter to prevent thundering herd
			jitter := time.Duration(float64(delay) * p.config.RetryConfig.RandomizationFactor * (2*rand.Float64() - 1))
			delay += jitter
		}

		p.logger.Warn("Retry attempt failed",
			zap.String("topic", topicName),
			zap.Int("attempt", retries),
			zap.Duration("next_delay", delay),
			zap.Error(err),
		)
	}

	return "", fmt.Errorf("failed after %d retries", retries)
}

// getTopic gets or creates a topic
func (p *PubSubPublisher) getTopic(ctx context.Context, topicName string) (*pubsub.Topic, error) {
	p.mu.RLock()
	topic, exists := p.topics[topicName]
	p.mu.RUnlock()

	if exists {
		return topic, nil
	}

	// Create topic if it doesn't exist in cache
	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	if topic, exists := p.topics[topicName]; exists {
		return topic, nil
	}

	fullTopicName := p.getFullTopicName(topicName)
	topic = p.client.Topic(fullTopicName)
	
	// Configure topic
	p.configureTopic(topic)

	// Check if topic exists
	exists, err := topic.Exists(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check topic existence: %w", err)
	}

	if !exists {
		// Try to create the topic
		_, err = p.client.CreateTopic(ctx, fullTopicName)
		if err != nil {
			// Topic might have been created by another process
			p.logger.Warn("Failed to create topic, continuing anyway",
				zap.String("topic", fullTopicName),
				zap.Error(err),
			)
		}
	}

	p.topics[topicName] = topic
	return topic, nil
}

// getFullTopicName returns the full topic name with prefix
func (p *PubSubPublisher) getFullTopicName(topicName string) string {
	if p.config.TopicPrefix != "" {
		return fmt.Sprintf("%s.%s", p.config.TopicPrefix, topicName)
	}
	return topicName
}

// serializeMessage serializes a message for publishing
func (p *PubSubPublisher) serializeMessage(message interface{}) ([]byte, error) {
	switch msg := message.(type) {
	case []byte:
		return msg, nil
	case string:
		return []byte(msg), nil
	case *Request:
		// Add metadata for Request type
		wrapper := map[string]interface{}{
			"id":         msg.ID,
			"type":       msg.IntegrationType,
			"timestamp":  msg.Timestamp,
			"trace_id":   msg.TraceID,
			"data":       msg,
		}
		return json.Marshal(wrapper)
	default:
		return json.Marshal(message)
	}
}

// createMessageAttributes creates Pub/Sub message attributes
func (p *PubSubPublisher) createMessageAttributes(message interface{}) map[string]string {
	attributes := make(map[string]string)
	
	// Add timestamp
	attributes["timestamp"] = time.Now().Format(time.RFC3339)
	
	// Add message ID
	attributes["message_id"] = uuid.New().String()
	
	// Add type information
	if req, ok := message.(*Request); ok {
		attributes["integration_type"] = req.IntegrationType
		attributes["trace_id"] = req.TraceID
		attributes["client_id"] = req.ClientID
	}
	
	return attributes
}

// extractOrderingKey extracts an ordering key from the message
func (p *PubSubPublisher) extractOrderingKey(message interface{}) string {
	// Extract ordering key based on message type
	if req, ok := message.(*Request); ok {
		// Use client ID as ordering key to maintain order per client
		return req.ClientID
	}
	
	return ""
}

// shouldRetry determines if an error should trigger a retry
func (p *PubSubPublisher) shouldRetry(err error) bool {
	// Implement logic to determine if error is retryable
	// For now, we'll consider timeout and temporary errors as retryable
	if err == context.DeadlineExceeded {
		return true
	}
	
	// Check for specific Pub/Sub errors that are retryable
	// This would need to be expanded based on actual error types
	
	return false
}

// recordSuccess records a successful publish
func (p *PubSubPublisher) recordSuccess(topicName string) {
	p.publishStats.mu.Lock()
	defer p.publishStats.mu.Unlock()
	
	p.publishStats.totalPublished++
	p.publishStats.lastPublishTime = time.Now()
	
	if stats, exists := p.publishStats.topicStats[topicName]; exists {
		stats.Published++
		stats.LastPublish = time.Now()
	} else {
		p.publishStats.topicStats[topicName] = &TopicStats{
			Published:   1,
			LastPublish: time.Now(),
		}
	}
}

// recordFailure records a failed publish
func (p *PubSubPublisher) recordFailure(topicName string, err error) {
	p.publishStats.mu.Lock()
	defer p.publishStats.mu.Unlock()
	
	p.publishStats.totalFailed++
	
	if stats, exists := p.publishStats.topicStats[topicName]; exists {
		stats.Failed++
		stats.LastError = err
	} else {
		p.publishStats.topicStats[topicName] = &TopicStats{
			Failed:    1,
			LastError: err,
		}
	}
}

// GetStats returns publishing statistics
func (p *PubSubPublisher) GetStats() map[string]interface{} {
	p.publishStats.mu.RLock()
	defer p.publishStats.mu.RUnlock()
	
	stats := make(map[string]interface{})
	stats["total_published"] = p.publishStats.totalPublished
	stats["total_failed"] = p.publishStats.totalFailed
	stats["last_publish_time"] = p.publishStats.lastPublishTime
	
	topicStats := make(map[string]interface{})
	for topic, ts := range p.publishStats.topicStats {
		topicStats[topic] = map[string]interface{}{
			"published":    ts.Published,
			"failed":       ts.Failed,
			"last_publish": ts.LastPublish,
		}
		if ts.LastError != nil {
			topicStats[topic].(map[string]interface{})["last_error"] = ts.LastError.Error()
		}
	}
	stats["topics"] = topicStats
	
	return stats
}

// IsHealthy returns the health status of the publisher
func (p *PubSubPublisher) IsHealthy() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.healthy
}

// healthCheck periodically checks the health of the publisher
func (p *PubSubPublisher) healthCheck() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		// Simple health check - try to get a topic
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		topic := p.client.Topic("health-check")
		exists, err := topic.Exists(ctx)
		cancel()
		
		p.mu.Lock()
		if err != nil {
			p.healthy = false
			p.logger.Warn("Publisher health check failed", zap.Error(err))
		} else {
			p.healthy = true
			if !exists {
				topic.Stop() // Clean up the topic reference
			}
		}
		p.mu.Unlock()
	}
}

// Close closes the publisher and releases resources
func (p *PubSubPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	
	// Stop all topics
	for _, topic := range p.topics {
		topic.Stop()
	}
	
	// Close the client
	if err := p.client.Close(); err != nil {
		return fmt.Errorf("failed to close pubsub client: %w", err)
	}
	
	p.healthy = false
	p.logger.Info("PubSub publisher closed")
	
	return nil
}
