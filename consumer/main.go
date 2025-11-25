package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Order represents the order message structure from Kafka
type Order struct {
	OrderID    string    `json:"order_id"`
	CustomerID string    `json:"customer_id"`
	ProductID  string    `json:"product_id"`
	Quantity   int       `json:"quantity"`
	Amount     float64   `json:"amount"`
	Timestamp  time.Time `json:"timestamp"`
}

// DatabaseConfig holds database connection settings
type DatabaseConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
	SSLMode  string
}

// ConsumerGroupHandler implements sarama.ConsumerGroupHandler with database integration
type ConsumerGroupHandler struct {
	ctx            context.Context
	db             *sql.DB
	processedCount atomic.Int64
	successCount   atomic.Int64
	failureCount   atomic.Int64
	workerPool     int
	batchSize      int
	messageChannel chan *sarama.ConsumerMessage
	wg             sync.WaitGroup
	statsInterval  time.Duration
}

// NewConsumerGroupHandler creates a new consumer group handler with database connection
func NewConsumerGroupHandler(ctx context.Context, db *sql.DB, workerPool, batchSize int) *ConsumerGroupHandler {
	handler := &ConsumerGroupHandler{
		ctx:            ctx,
		db:             db,
		workerPool:     workerPool,
		batchSize:      batchSize,
		messageChannel: make(chan *sarama.ConsumerMessage, batchSize*workerPool),
		statsInterval:  10 * time.Second,
	}

	// Start worker goroutines for concurrent processing
	for i := 0; i < workerPool; i++ {
		handler.wg.Add(1)
		go handler.worker(i)
	}

	// Start statistics reporter
	go handler.reportStats()

	return handler
}

// Setup is called at the beginning of a new session, before ConsumeClaim
func (h *ConsumerGroupHandler) Setup(sarama.ConsumerGroupSession) error {
	log.Println("✓ Consumer group session started")
	return nil
}

// Cleanup is called at the end of a session, once all ConsumeClaim goroutines have exited
func (h *ConsumerGroupHandler) Cleanup(sarama.ConsumerGroupSession) error {
	log.Println("✓ Consumer group session ended")
	return nil
}

// ConsumeClaim processes messages from a partition
func (h *ConsumerGroupHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	batch := make([]*sarama.ConsumerMessage, 0, h.batchSize)

	for {
		select {
		case <-session.Context().Done():
			// Process remaining batch before exiting
			if len(batch) > 0 {
				h.processBatch(batch)
				session.MarkMessage(batch[len(batch)-1], "")
			}
			return nil

		case message := <-claim.Messages():
			if message == nil {
				continue
			}

			// Add message to batch
			batch = append(batch, message)

			// Process batch when full
			if len(batch) >= h.batchSize {
				h.processBatch(batch)
				session.MarkMessage(batch[len(batch)-1], "") // Commit offset
				batch = batch[:0]                            // Reset batch
			}
		}
	}
}

// processBatch sends batch to worker pool for parallel processing
func (h *ConsumerGroupHandler) processBatch(messages []*sarama.ConsumerMessage) {
	for _, msg := range messages {
		select {
		case h.messageChannel <- msg:
		case <-h.ctx.Done():
			return
		}
	}
}

// worker processes messages from the channel with database transactions
func (h *ConsumerGroupHandler) worker(id int) {
	defer h.wg.Done()

	log.Printf("Worker %d started", id)

	for {
		select {
		case msg := <-h.messageChannel:
			if msg == nil {
				continue
			}
			h.processMessage(msg, id)

		case <-h.ctx.Done():
			log.Printf("Worker %d shutting down", id)
			return
		}
	}
}

// processMessage handles individual message processing with database transaction
func (h *ConsumerGroupHandler) processMessage(msg *sarama.ConsumerMessage, workerID int) {
	var order Order

	// Unmarshal JSON
	if err := json.Unmarshal(msg.Value, &order); err != nil {
		log.Printf("Worker %d: Failed to unmarshal message: %v", workerID, err)
		h.failureCount.Add(1)
		return
	}

	// Process order with database transaction (inventory check and booking)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := h.reserveInventoryAndCreateOrder(ctx, &order)
	if err != nil {
		log.Printf("Worker %d: Failed to process order %s: %v", workerID, order.OrderID, err)
		h.failureCount.Add(1)
		// In production: send to Dead Letter Queue (DLQ)
		return
	}

	// Success
	h.successCount.Add(1)
	h.processedCount.Add(1)
}

// reserveInventoryAndCreateOrder performs the critical inventory check and reservation
// This function demonstrates the SELECT FOR UPDATE pattern to prevent race conditions
func (h *ConsumerGroupHandler) reserveInventoryAndCreateOrder(ctx context.Context, order *Order) error {
	// Begin database transaction with READ COMMITTED isolation level
	// This prevents dirty reads while allowing high concurrency
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelReadCommitted,
	})
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() // Rollback if we don't reach Commit()

	// ============================================================
	// STEP 1: SELECT FOR UPDATE - Lock the inventory row
	// ============================================================
	// This is the CRITICAL concurrency control mechanism:
	// - Acquires an EXCLUSIVE ROW-LEVEL LOCK on the inventory row
	// - Prevents other transactions from modifying the same row
	// - NOWAIT fails immediately if row is already locked (fast failure)
	// - FOR UPDATE ensures no double-booking can occur
	// ============================================================

	var availableQty int
	var unitPrice float64

	selectQuery := `
		SELECT available_quantity, unit_price
		FROM inventory
		WHERE product_id = $1
		FOR UPDATE NOWAIT
	`

	err = tx.QueryRowContext(ctx, selectQuery, order.ProductID).Scan(&availableQty, &unitPrice)
	if err == sql.ErrNoRows {
		return fmt.Errorf("product not found: %s", order.ProductID)
	}
	if err != nil {
		// Check if error is due to lock timeout (row already locked)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			// PostgreSQL error code 55P03: lock_not_available
			if pgErr.Code == "55P03" {
				return fmt.Errorf("inventory locked by another transaction, retry later")
			}
		}
		return fmt.Errorf("failed to query inventory: %w", err)
	}

	// ============================================================
	// STEP 2: Check inventory availability
	// ============================================================
	// At this point, we have an EXCLUSIVE LOCK on the row
	// No other transaction can read or modify this inventory until we commit/rollback
	// ============================================================

	if availableQty < order.Quantity {
		// Insufficient inventory - rollback transaction (releases lock)
		return fmt.Errorf("insufficient inventory for product %s: need %d, have %d",
			order.ProductID, order.Quantity, availableQty)
	}

	// ============================================================
	// STEP 3: Reserve inventory (decrement available, increment reserved)
	// ============================================================
	// This UPDATE happens while we still hold the lock
	// No race condition possible - we're guaranteed to be the only transaction modifying this row
	// ============================================================

	updateInventoryQuery := `
		UPDATE inventory
		SET available_quantity = available_quantity - $1,
		    reserved_quantity = reserved_quantity + $1,
		    updated_at = NOW()
		WHERE product_id = $2
	`

	result, err := tx.ExecContext(ctx, updateInventoryQuery, order.Quantity, order.ProductID)
	if err != nil {
		return fmt.Errorf("failed to update inventory: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("inventory update affected 0 rows for product %s", order.ProductID)
	}

	// ============================================================
	// STEP 4: Create order record
	// ============================================================
	// Insert order with 'reserved' status
	// This happens in the same transaction, so if this fails, inventory is rolled back
	// ============================================================

	insertOrderQuery := `
		INSERT INTO orders (order_id, customer_id, product_id, quantity, amount, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'reserved', NOW(), NOW())
	`

	_, err = tx.ExecContext(ctx, insertOrderQuery,
		order.OrderID,
		order.CustomerID,
		order.ProductID,
		order.Quantity,
		order.Amount,
	)
	if err != nil {
		return fmt.Errorf("failed to insert order: %w", err)
	}

	// ============================================================
	// STEP 5: Commit transaction
	// ============================================================
	// Commit releases the row lock and makes changes visible to other transactions
	// If commit fails (network error, constraint violation), everything is rolled back
	// ============================================================

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// Success! Inventory reserved and order created atomically
	log.Printf("✓ Order %s processed: Customer=%s, Product=%s, Qty=%d, Amount=%.2f",
		order.OrderID, order.CustomerID, order.ProductID, order.Quantity, order.Amount)

	return nil
}

// reportStats prints processing statistics periodically
func (h *ConsumerGroupHandler) reportStats() {
	ticker := time.NewTicker(h.statsInterval)
	defer ticker.Stop()

	var lastProcessed int64

	for {
		select {
		case <-ticker.C:
			processed := h.processedCount.Load()
			success := h.successCount.Load()
			failure := h.failureCount.Load()

			throughput := float64(processed-lastProcessed) / h.statsInterval.Seconds()
			successRate := 0.0
			if processed > 0 {
				successRate = float64(success) / float64(processed) * 100
			}

			log.Printf("📊 Stats: Processed=%d, Success=%d, Failed=%d, Throughput=%.0f msg/sec, Success Rate=%.2f%%",
				processed, success, failure, throughput, successRate)

			lastProcessed = processed

		case <-h.ctx.Done():
			return
		}
	}
}

// Close stops all workers and waits for completion
func (h *ConsumerGroupHandler) Close() {
	close(h.messageChannel)
	h.wg.Wait()
}

// initDatabase initializes PostgreSQL connection pool
func initDatabase(config DatabaseConfig) (*sql.DB, error) {
	// Build connection string
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		config.Host, config.Port, config.User, config.Password, config.Database, config.SSLMode)

	// Open connection (pgx driver)
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool for high throughput
	db.SetMaxOpenConns(100)                 // Maximum concurrent connections
	db.SetMaxIdleConns(20)                  // Keep 20 idle connections ready
	db.SetConnMaxLifetime(time.Hour)        // Recycle connections every hour
	db.SetConnMaxIdleTime(10 * time.Minute) // Close idle connections after 10 min

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = db.PingContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Println("✓ Database connection established")
	return db, nil
}

func main() {
	// Read configuration from environment variables
	brokers := getEnvOrDefault("KAFKA_BROKERS", "kafka:9092")
	topic := getEnvOrDefault("KAFKA_TOPIC", "orders-placed")
	groupID := getEnvOrDefault("CONSUMER_GROUP_ID", "order-inventory-group")

	// Database configuration
	dbConfig := DatabaseConfig{
		Host:     getEnvOrDefault("DB_HOST", "postgres"),
		Port:     getEnvOrDefault("DB_PORT", "5432"),
		User:     getEnvOrDefault("DB_USER", "orderapp"),
		Password: getEnvOrDefault("DB_PASSWORD", "changeme"),
		Database: getEnvOrDefault("DB_NAME", "postgres"),
		SSLMode:  getEnvOrDefault("DB_SSLMODE", "disable"),
	}

	// Worker pool configuration
	workerPool := 50 // 50 concurrent goroutines per consumer instance
	batchSize := 100 // Process messages in batches of 100

	log.Printf("Starting Order Inventory Consumer")
	log.Printf("Kafka: brokers=%s, topic=%s, group=%s", brokers, topic, groupID)
	log.Printf("Database: host=%s, port=%s, database=%s", dbConfig.Host, dbConfig.Port, dbConfig.Database)
	log.Printf("Workers: pool=%d, batch=%d", workerPool, batchSize)

	// Initialize database connection
	db, err := initDatabase(dbConfig)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()

	// Configure Sarama for high-throughput consumption
	config := sarama.NewConfig()
	config.Version = sarama.V3_0_0_0                                        // Kafka version
	config.Consumer.Group.Rebalance.Strategy = sarama.BalanceStrategySticky // Sticky rebalancing
	config.Consumer.Offsets.Initial = sarama.OffsetNewest                   // Start from latest for new consumers
	config.Consumer.Offsets.AutoCommit.Enable = false                       // Manual commit for exactly-once
	config.Consumer.Fetch.Min = 1024 * 1024                                 // 1MB minimum fetch size
	config.Consumer.Fetch.Default = 10 * 1024 * 1024                        // 10MB default fetch
	config.Consumer.MaxProcessingTime = 10 * time.Second                    // Max processing time before rebalance
	config.Consumer.Return.Errors = true
	config.Consumer.MaxWaitTime = 500 * time.Millisecond // Max wait for min bytes
	config.Net.ReadTimeout = 30 * time.Second
	config.Net.WriteTimeout = 30 * time.Second

	// Create consumer group
	consumerGroup, err := sarama.NewConsumerGroup([]string{brokers}, groupID, config)
	if err != nil {
		log.Fatalf("Failed to create consumer group: %v", err)
	}
	defer consumerGroup.Close()

	// Setup context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create handler with database connection
	handler := NewConsumerGroupHandler(ctx, db, workerPool, batchSize)
	defer handler.Close()

	// Error handling goroutine
	go func() {
		for err := range consumerGroup.Errors() {
			log.Printf("❌ Consumer error: %v", err)
		}
	}()

	// Consume in goroutine
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			// `Consume` should be called inside an infinite loop
			// When a rebalance happens, this function returns and needs to be called again
			if err := consumerGroup.Consume(ctx, []string{topic}, handler); err != nil {
				log.Printf("Error from consumer: %v", err)
			}

			// Check if context was cancelled
			if ctx.Err() != nil {
				return
			}
		}
	}()

	log.Println("✓ Consumer started successfully - Processing orders with inventory reservation")

	// Wait for interrupt signal
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigterm:
		log.Println("Received termination signal")
	case <-ctx.Done():
		log.Println("Context cancelled")
	}

	// Graceful shutdown
	log.Println("Shutting down consumer...")
	cancel()
	wg.Wait()

	log.Println("✓ Consumer stopped gracefully")
}

// getEnvOrDefault returns environment variable or default value
func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
