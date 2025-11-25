package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Order represents the incoming order structure
type Order struct {
	OrderID    string    `json:"order_id"`
	CustomerID string    `json:"customer_id" binding:"required"`
	ProductID  string    `json:"product_id" binding:"required"`
	Quantity   int       `json:"quantity" binding:"required,min=1"`
	Amount     float64   `json:"amount" binding:"required,min=0"`
	Timestamp  time.Time `json:"timestamp"`
}

// ProducerService handles Kafka producer operations
type ProducerService struct {
	producer sarama.AsyncProducer
	topic    string
}

// NewProducerService creates a new producer service with optimized configuration
func NewProducerService(brokers []string, topic string) (*ProducerService, error) {
	config := sarama.NewConfig()

	// Producer optimization for high throughput
	config.Producer.RequiredAcks = sarama.WaitForAll       // acks=all: required for idempotent producer
	config.Producer.Compression = sarama.CompressionSnappy // Fast compression, reduces network I/O
	config.Producer.Flush.Messages = 1000                  // Batch 1000 messages
	config.Producer.Flush.MaxMessages = 1000
	config.Producer.Flush.Frequency = 10 * time.Millisecond // Max 10ms batching delay
	config.Producer.Return.Successes = false                // Async mode: don't wait for success
	config.Producer.Return.Errors = true                    // But do track errors
	config.Producer.MaxMessageBytes = 1048576               // 1MB max message size
	config.Net.MaxOpenRequests = 1                          // Must be 1 for idempotent producer
	config.Producer.Idempotent = true                       // Enable idempotence for exactly-once
	config.Producer.Retry.Max = 3                           // Retry failed sends 3 times
	config.Version = sarama.V3_0_0_0                        // Use recent Kafka version

	// Create async producer for maximum throughput
	producer, err := sarama.NewAsyncProducer(brokers, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create producer: %w", err)
	}

	// Start error handler goroutine
	go func() {
		for err := range producer.Errors() {
			log.Printf("Producer error: %v", err)
		}
	}()

	return &ProducerService{
		producer: producer,
		topic:    topic,
	}, nil
}

// PublishOrder sends an order to Kafka
func (ps *ProducerService) PublishOrder(order *Order) error {
	// Marshal order to JSON
	orderJSON, err := json.Marshal(order)
	if err != nil {
		return fmt.Errorf("failed to marshal order: %w", err)
	}

	// Use customer_id as partition key to ensure ordering per customer
	key := []byte(order.CustomerID)

	// Create producer message
	msg := &sarama.ProducerMessage{
		Topic: ps.topic,
		Key:   sarama.ByteEncoder(key),
		Value: sarama.ByteEncoder(orderJSON),
	}

	// Send message asynchronously (non-blocking)
	ps.producer.Input() <- msg

	return nil
}

// Close shuts down the producer gracefully
func (ps *ProducerService) Close() error {
	return ps.producer.Close()
}

func main() {
	// Read configuration from environment variables
	brokers := getEnvOrDefault("KAFKA_BROKERS", "kafka:9092")
	topic := getEnvOrDefault("KAFKA_TOPIC", "orders-placed")
	port := getEnvOrDefault("PORT", "8080")

	// Initialize Kafka producer
	producerSvc, err := NewProducerService([]string{brokers}, topic)
	if err != nil {
		log.Fatalf("Failed to initialize producer: %v", err)
	}
	defer producerSvc.Close()

	// Initialize Gin router
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	// Health check endpoint
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy"})
	})

	// Order submission endpoint
	router.POST("/order", func(c *gin.Context) {
		var order Order

		// Bind and validate JSON request
		if err := c.ShouldBindJSON(&order); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Generate order ID and timestamp
		order.OrderID = uuid.New().String()
		order.Timestamp = time.Now()

		// Publish to Kafka asynchronously
		if err := producerSvc.PublishOrder(&order); err != nil {
			log.Printf("Failed to publish order: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process order"})
			return
		}

		// Respond immediately (don't wait for Kafka ack)
		c.JSON(http.StatusAccepted, gin.H{
			"order_id": order.OrderID,
			"status":   "accepted",
		})
	})

	// Batch order submission endpoint for load testing
	router.POST("/orders/batch", func(c *gin.Context) {
		var orders []Order

		if err := c.ShouldBindJSON(&orders); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Process all orders
		for i := range orders {
			orders[i].OrderID = uuid.New().String()
			orders[i].Timestamp = time.Now()

			if err := producerSvc.PublishOrder(&orders[i]); err != nil {
				log.Printf("Failed to publish order: %v", err)
			}
		}

		c.JSON(http.StatusAccepted, gin.H{
			"count":  len(orders),
			"status": "accepted",
		})
	})

	// Start HTTP server with graceful shutdown
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Run server in goroutine
	go func() {
		log.Printf("Producer API starting on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited")
}

// getEnvOrDefault returns environment variable or default value
func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
