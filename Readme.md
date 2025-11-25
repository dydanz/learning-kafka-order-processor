# High-Throughput Order Processing System with Kafka & PostgreSQL

This is a learning project where I'm building a production-ready order processing system using **Apache Kafka**, **PostgreSQL**, **Go**, and **Docker**. The goal? Handle **1 million transactions per second (1M TPS)** while making sure we don't accidentally double-book orders (because that would be bad).

## What This Project Does

Ever wondered how e-commerce platforms handle flash sales without overselling? This system shows how to:

- **High Throughput**: Aim for 1M TPS using async producers, smart batching, and connection pooling
- **Concurrency Control**: PostgreSQL's row-level locking (`SELECT FOR UPDATE`) keeps us from selling the same product twice
- **Fault Tolerant**: With Kafka replication, PostgreSQL streaming replication, and graceful shutdowns, this thing can take a hit
- **Production Ready**: Health checks, monitoring, structured logging, and proper error handling - all the good stuff
- **Horizontally Scalable**: Need more power? Just add more consumers, brokers, or database replicas

## How It All Works

### The Journey of an Order

```
Customer → Producer API (Gin/Go) → Kafka → Consumer Workers → PostgreSQL
           ↓ POST /order        ↓ orders-placed  ↓ SELECT FOR UPDATE  ↓
           ✓ Order Accepted      ✓ Partitioned    ✓ Inventory Reserved  ✓ Order Stored
```

### The Players

| Component     | Technology         | What It Does                                     | How It Scales        |
|---------------|--------------------|--------------------------------------------------|----------------------|
| **Producer**  | Go, Gin, Sarama    | REST API where customers submit orders           | Load balanced (N instances) |
| **Kafka**     | Confluent Kafka    | Message streaming with 50 partitions per topic   | 50+ brokers for 1M TPS |
| **Consumer**  | Go, Sarama, pgx    | Processes orders and reserves inventory          | 500+ instances       |
| **Database**  | PostgreSQL 15      | Stores everything with row-level locking         | Primary + 5 replicas |
| **Monitoring**| Kafka UI           | Real-time monitoring of topics and consumers     | -                    |

---

## Let's Get This Running

### 1. Fire Up the Engines

```bash
# Start everything (Zookeeper, Kafka, PostgreSQL, Producer, Consumer)
docker-compose up -d

# Make sure everything's healthy
docker-compose ps

# Watch what's happening
docker-compose logs -f producer
docker-compose logs -f consumer
docker-compose logs -f postgres
```

### 2. Check the Database

```bash
# Jump into PostgreSQL
docker exec -it postgres psql -U postgres

# See what tables we've got
\dt

# Look at our starting inventory
SELECT product_id, product_name, available_quantity, reserved_quantity FROM inventory;

# All done
\q
```

You should see 10 products (PROD001 through PROD010) with their initial inventory ready to go.

### 3. Place Some Orders

**Single Order:**
```bash
curl -X POST http://localhost:8080/order \
  -H "Content-Type: application/json" \
  -d '{
    "customer_id": "CUST001",
    "product_id": "PROD001",
    "quantity": 5,
    "amount": 6499.95
  }'
```

**You'll Get Back:**
```json
{
  "order_id": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
  "status": "accepted"
}
```

**Batch Orders (For When You Want to Stress Test):**
```bash
curl -X POST http://localhost:8080/orders/batch \
  -H "Content-Type: application/json" \
  -d '[
    {"customer_id": "CUST001", "product_id": "PROD001", "quantity": 1, "amount": 1299.99},
    {"customer_id": "CUST002", "product_id": "PROD002", "quantity": 10, "amount": 299.90},
    {"customer_id": "CUST003", "product_id": "PROD003", "quantity": 5, "amount": 64.95}
  ]'
```

### 4. Make Sure It Worked

**Check the Consumer Logs:**
```bash
docker-compose logs consumer | grep "Order.*processed"
```

You should see something like:
```
✓ Order f47ac10b-58cc-4372-a567-0e02b2c3d479 processed: Customer=CUST001, Product=PROD001, Qty=5, Amount=6499.95
```

**Check the Database:**
```bash
docker exec -it postgres psql -U postgres -c "SELECT order_id, customer_id, product_id, quantity, status FROM orders ORDER BY created_at DESC LIMIT 5;"
```

**See How Inventory Changed:**
```bash
docker exec -it postgres psql -U postgres -c "SELECT product_id, available_quantity, reserved_quantity FROM inventory WHERE product_id = 'PROD001';"
```

The `available_quantity` should be lower and `reserved_quantity` should be higher. Magic!

### 5. Check Out the Monitoring Dashboard

Open http://localhost:8090 in your browser and you'll see:
- **Topics**: orders-placed, orders-validated, orders-inventory, orders-warehouse
- **Consumer Groups**: How much lag there is, offsets, partition assignments
- **Messages**: Browse and search through actual messages

---

## Testing for Race Conditions (The Fun Part)

This is where we prove that `SELECT FOR UPDATE` actually works. Let's try to break it!

### Hammering the Same Product

```bash
# Fire off 100 orders for the same product at once
for i in {1..100}; do
  curl -X POST http://localhost:8080/order \
    -H "Content-Type: application/json" \
    -d "{\"customer_id\":\"CUST$i\",\"product_id\":\"PROD007\",\"quantity\":1,\"amount\":199.99}" &
done
wait

# Now let's see if we messed up
docker exec -it postgres psql -U postgres -c "
  SELECT
    product_id,
    available_quantity,
    reserved_quantity,
    (SELECT COUNT(*) FROM orders WHERE product_id = 'PROD007' AND status = 'reserved') AS orders_count
  FROM inventory
  WHERE product_id = 'PROD007';
"
```

**What Should Happen:**
- `reserved_quantity` should EXACTLY match `orders_count` (no more, no less)
- The math should work out: `available_quantity + reserved_quantity = total_quantity`

**How SELECT FOR UPDATE Saves the Day:**

1. **Transaction T1** grabs `SELECT ... FOR UPDATE` on PROD007
   - PostgreSQL locks that row exclusively
2. **Transaction T2** (happening at the same time) tries to do the same
   - PostgreSQL makes T2 wait
3. **T1 commits** and releases the lock
4. **T2 finally gets in** and sees the updated inventory

**What Would Happen Without It:**
```
T1 reads: available=100
T2 reads: available=100  ← Uh oh, both see the same number
T1 updates: available=99
T2 updates: available=99 ← T2 just overwrote T1's work (DISASTER)
```

**With SELECT FOR UPDATE:**
```
T1 reads + locks: available=100
T2 waits...         ← No soup for you until T1 is done
T1 updates: available=99
T1 commits → lock released
T2 reads + locks: available=99  ← Sees the correct value
T2 updates: available=98
T2 commits
```

---

## Scaling This Thing to the Moon

### Adding More Workers

**More Consumers (Good rule of thumb: 1 consumer per 2-3 partitions):**
```bash
docker-compose up -d --scale consumer=10
```

**More Producers (Put them behind a load balancer):**
```bash
docker-compose up -d --scale producer=5
```

**More Kafka Brokers:**

Edit `docker-compose.yml` and add more brokers:
```yaml
kafka-2:
  image: confluentinc/cp-kafka:7.5.0
  environment:
    KAFKA_BROKER_ID: 2
    # ... same config as kafka-1
```

### The Dream Setup for 1M TPS

Big thanks to AI for helping me figure out these numbers!

**What You'd Need:**

| Component       | Configuration                          | Recommended Value          |
|-----------------|----------------------------------------|----------------------------|
| Kafka Brokers   | Total brokers                          | 50-60 brokers              |
| Kafka Partitions| `orders-placed` partitions             | 100 partitions             |
| Consumer Instances | Total consumer instances            | 500 instances              |
| Producer Instances | Load-balanced producer instances    | 10 instances               |
| Database        | PostgreSQL Primary + Replicas          | 1 primary + 5 replicas     |
| Database Connections | Max connections per instance       | 100 connections            |

**Hardware Per Broker (For 1M TPS):**
- CPU: 16 cores (Intel Xeon Gold or AMD EPYC)
- RAM: 64 GB
- Storage: 8 TB NVMe SSD (RAID 10)
- Network: 25 Gbps NICs

**PostgreSQL Primary Node:**
- CPU: 32 cores
- RAM: 256 GB
- Storage: 20 TB NVMe SSD
- IOPS: 750,000+ sustained

*\*\*Note: I haven't actually built this mega-setup in real life. The time the code was committed to GitHub, I was still running benchmark tests on Mac M4.*

---

## Making It Fast (Really Fast)

### Producer Tricks (Already Built In)

```go
// Async mode with batching
config.Producer.RequiredAcks = sarama.WaitForAll  // acks=all
config.Producer.Compression = sarama.CompressionSnappy  // 3:1 compression ratio
config.Producer.Flush.Messages = 1000  // Batch up 1000 messages
config.Producer.Flush.Frequency = 10 * time.Millisecond  // Wait max 10ms
config.Producer.Return.Successes = false  // Don't wait for confirmation
```

**What This Gets You**: About 10x better throughput than synchronous mode.

### Consumer Optimizations (Also Built In)

```go
// Fetch bigger chunks at a time
config.Consumer.Fetch.Min = 1024 * 1024  // 1MB minimum
config.Consumer.Fetch.Default = 10 * 1024 * 1024  // 10MB default

// Worker pool to process in parallel
workerPool := 50  // 50 goroutines per consumer instance
batchSize := 100  // Process 100 messages per batch
```

**What This Gets You**: Way fewer network trips (99% reduction) and maxed out CPU usage.

### Database Optimizations (Yep, Also Built In)

**Connection Pooling:**
```go
db.SetMaxOpenConns(100)  // 100 concurrent connections
db.SetMaxIdleConns(20)   // Keep 20 warm and ready
```

**Transaction Isolation:**
```sql
-- Prevents dirty reads while still allowing good concurrency
SET TRANSACTION ISOLATION LEVEL READ COMMITTED;
```

**Smart Indexes (See database/schema.sql):**
```sql
CREATE INDEX idx_inventory_product_id ON inventory(product_id);
CREATE INDEX idx_orders_customer ON orders(customer_id, created_at DESC);
```

---

## Keeping an Eye on Things

### What You Should Watch

| Metric                  | Target/Threshold          | When to Freak Out                 | What It Means                   |
|-------------------------|---------------------------|-----------------------------------|---------------------------------|
| **Consumer Lag**        | < 100,000 messages        | Alert if > 100K for 5 minutes     | Orders are piling up            |
| **Database Latency**    | p99 < 100ms               | Alert if p99 > 200ms              | Database is struggling          |
| **Lock Wait Time**      | < 50ms average            | Alert if avg > 50ms               | Too much fighting over products |
| **Network I/O**         | < 70% of 25 Gbps          | Alert if > 70%                    | Need more brokers               |
| **Error Rate**          | < 0.1%                    | Alert if > 0.1%                   | Something's broken              |

### Where to Look

**Kafka UI (Pretty Web Interface):**
- URL: http://localhost:8090
- See: Topic lag, consumer groups, partition distribution, actual messages

**PostgreSQL Monitoring:**
```sql
-- How many connections are active?
SELECT count(*) FROM pg_stat_activity WHERE state = 'active';

-- Anyone waiting for locks?
SELECT * FROM pg_locks WHERE NOT granted;

-- Which queries are slow?
SELECT queryid, mean_exec_time, calls
FROM pg_stat_statements
ORDER BY mean_exec_time DESC
LIMIT 10;
```

**Consumer Stats:**
```bash
# See how fast we're processing (logged every 10 seconds)
docker-compose logs consumer | grep "Stats:"
```

You'll see something like:
```
📊 Stats: Processed=15000, Success=14950, Failed=50, Throughput=1500 msg/sec, Success Rate=99.67%
```

---

## When Things Go Wrong (And How to Fix Them)

### Staying Up When Parts Fail

**Kafka High Availability:**
- **Replication Factor**: 3 (every partition has 3 copies)
- **Min In-Sync Replicas**: 2 (need 2 brokers to say "yep, got it")
- **Automatic Failover**: New leader elected in under 10 seconds

**PostgreSQL High Availability:**
- **Streaming Replication**: Real-time sync to 1 standby (no data loss)
- **Automatic Failover**: Using Patroni/Stolon (back up in under 30 seconds)
- **Read Replicas**: 5 replicas for read-heavy queries

**When Stuff Crashes:**

| What Dies                | Impact                    | How Fast We Recover           |
|--------------------------|---------------------------|-------------------------------|
| Single consumer crash    | None (auto-rebalance)     | < 10 seconds                  |
| Single broker crash      | None (replicas take over) | 0 seconds (seamless)          |
| Database primary crash   | Promote standby           | < 30 seconds (Patroni magic)  |
| Entire datacenter fails  | Failover to DR DC         | < 5 minutes (MirrorMaker2)    |

### Backup Plan

**PostgreSQL Backups:**
```bash
# Continuous archiving (Write-Ahead Logs)
archive_mode = on
archive_command = 'cp %p /archive/%f'

# Daily full backup
docker exec postgres pg_basebackup -D /backup -Ft -z -P

# Point-in-time recovery
# Restore from base backup + replay WAL logs to any point in time
```

---

## Development & Testing Stuff

### Running Tests

**Unit Tests (Go):**
```bash
cd producer
go test -v ./...

cd ../consumer
go test -v ./...
```

**Integration Tests:**
```bash
# Start everything
docker-compose up -d

# Give it a moment to wake up
sleep 30

# Run the tests
./scripts/integration-test.sh
```

**Load Testing (Apache Bench):**
```bash
# Throw 10,000 requests at it with 100 concurrent connections
ab -n 10000 -c 100 -p order.json -T application/json http://localhost:8080/order
```

**Load Testing (k6 - The Fancy Way):**
```bash
# Install k6
brew install k6

# Simulate 1000 users for 60 seconds
k6 run --vus 1000 --duration 60s scripts/load-test.js
```

### Spying on Database Queries

```bash
# Query logging is already on in docker-compose.yml
log_statement = all
log_duration = on

# Find the slow ones
docker exec -it postgres psql -U postgres -c "
  SELECT query, mean_exec_time, calls
  FROM pg_stat_statements
  WHERE mean_exec_time > 100
  ORDER BY mean_exec_time DESC;
"
```

---

## Common Problems and How to Fix Them

### Consumer Lag is Growing

**The Problem:** Consumer lag > 100K messages

**Figure Out What's Wrong:**
```bash
# Look for errors in consumer logs
docker-compose logs consumer | grep "Failed"

# Check database connections
docker exec -it postgres psql -U postgres -c "SELECT count(*) FROM pg_stat_activity WHERE usename = 'orderapp';"
```

**The Fix:**
1. Add more consumers: `docker-compose up -d --scale consumer=10`
2. Bump up the worker pool: Edit `consumer/main.go` → `workerPool := 100`
3. Check database performance: Maybe add some indexes or increase the connection pool

### Database is Locked Up

**The Problem:** Everything's slow, lots of lock waiting

**Figure Out What's Wrong:**
```sql
-- See who's blocking who
SELECT
  blocked_locks.pid AS blocked_pid,
  blocked_activity.usename AS blocked_user,
  blocking_locks.pid AS blocking_pid,
  blocking_activity.usename AS blocking_user,
  blocked_activity.query AS blocked_statement
FROM pg_locks blocked_locks
JOIN pg_stat_activity blocked_activity ON blocked_activity.pid = blocked_locks.pid
JOIN pg_locks blocking_locks ON blocking_locks.locktype = blocked_locks.locktype
JOIN pg_stat_activity blocking_activity ON blocking_activity.pid = blocking_locks.pid
WHERE NOT blocked_locks.granted;
```

**The Fix:**
1. Make transactions faster: Optimize queries, add indexes
2. Spread out hot products: Distribute popular inventory across multiple rows
3. Try optimistic locking for products that aren't super popular

### Producer is Complaining

**The Problem:** "Producer error" messages in the logs

**Figure Out What's Wrong:**
```bash
docker-compose logs producer | grep "Producer error"
```

**Common Causes:**
- Kafka broker is down → Check `docker-compose ps kafka`
- Network timeout → Increase `config.Net.WriteTimeout`
- Message is too big → Increase `config.Producer.MaxMessageBytes`

---

## What's Where

```
order-processor-using-kafka/
├── producer/
│   ├── main.go              # Gin API with async Kafka producer
│   ├── Dockerfile           # Multi-stage build for producer
│   ├── go.mod               # Go dependencies
│   └── go.sum
├── consumer/
│   ├── main.go              # Consumer with SELECT FOR UPDATE magic
│   ├── Dockerfile           # Multi-stage build for consumer
│   ├── go.mod               # Go dependencies (includes pgx)
│   └── go.sum
├── database/
│   └── schema.sql           # PostgreSQL schema (orders, inventory tables)
├── docker-compose.yml       # Orchestration for all services
├── Readme.md                # You are here!
├── ARCHITECTURE.md          # Deep dive into design decisions
└── .gitignore
```

---

## Why I Built It This Way (Top 5 Decisions)

### 1. **Partitioning by `customer_id`**

**What I Did:** Use `customer_id` as the Kafka partition key.

**Why:**
- All orders from the same customer end up on the same partition
- Keeps strict ordering per customer (important!)
- Prevents weird race conditions on customer-specific stuff like credit limits

**The Code:**
```go
key := []byte(order.CustomerID)  // This determines the partition
msg := &sarama.ProducerMessage{
    Topic: "orders-placed",
    Key:   sarama.ByteEncoder(key),
    Value: orderJSON,
}
```

---

### 2. **SELECT FOR UPDATE for Concurrency Control**

**What I Did:** Used pessimistic locking (row-level locks) instead of optimistic locking.

**Why:**
- **High Contention**: At 1M TPS, optimistic locking would cause tons of retries (waste of CPU)
- **Data Integrity**: The database guarantees we can't double-book (ACID for the win)
- **PostgreSQL Handles Deadlocks**: It automatically detects and resolves them

**Trade-offs:**
- More lock waiting under heavy load (but transactions are fast, so it's okay)
- Wouldn't work well with distributed databases like Cassandra or DynamoDB

**The Code:**
```sql
SELECT available_quantity FROM inventory
WHERE product_id = $1
FOR UPDATE NOWAIT;  -- Fail immediately if someone else has it locked
```

---

### 3. **Async Producer with Batching**

**What I Did:** Use async producer with 1000-message batches and 10ms wait time.

**Why:**
- **Throughput**: 10x better than synchronous mode
- **Latency**: 10ms delay is totally fine for async order processing
- **Compression**: Batching means better compression (3:1 with Snappy)

**Trade-offs:**
- Fire-and-forget: No immediate confirmation (we track errors in the background)
- Buffering: Messages wait up to 10ms before being sent

**The Configuration:**
```go
config.Producer.Flush.Messages = 1000
config.Producer.Flush.Frequency = 10 * time.Millisecond
config.Producer.Compression = sarama.CompressionSnappy
```

---

### 4. **Worker Pool Pattern in Consumer**

**What I Did:** Use 50 goroutines per consumer instance to process messages in parallel.

**Why:**
- **Concurrency**: Database I/O is slow (10-100ms), so workers can process other stuff while waiting
- **Resource Control**: Prevents spawning a bajillion goroutines
- **Offset Management**: Batch commit offsets after processing 100 messages

**How It Works:**
```go
for i := 0; i < workerPool; i++ {
    go handler.worker(i)  // Spin up 50 workers
}
```

---

### 5. **Manual Offset Commits with Batching**

**What I Did:** Turned off auto-commit, commit offsets manually after processing 100 messages.

**Why:**
- **Exactly-Once Semantics**: Only commit after the database transaction succeeds
- **Performance**: Batch commits reduce overhead by 99% (1 commit per 100 messages instead of per message)
- **At-Least-Once Delivery**: If we crash, we reprocess the last batch (fine for idempotent operations)

**The Configuration:**
```go
config.Consumer.Offsets.AutoCommit.Enable = false
// Manual commit:
session.MarkMessage(batch[len(batch)-1], "")
```

---

## Helpful Resources

**Documentation:**
- [ARCHITECTURE.md](ARCHITECTURE.md) - Deep dive into design (broker sizing, partitioning, monitoring)
- [Kafka Documentation](https://kafka.apache.org/documentation/)
- [PostgreSQL Documentation](https://www.postgresql.org/docs/)
- [Sarama Library](https://github.com/IBM/sarama)
- [pgx Library](https://github.com/jackc/pgx)

**Performance Tuning:**
- [Kafka Performance Tuning](https://docs.confluent.io/platform/current/installation/configuration/producer-configs.html)
- [PostgreSQL Performance Tips](https://wiki.postgresql.org/wiki/Performance_Optimization)

---
