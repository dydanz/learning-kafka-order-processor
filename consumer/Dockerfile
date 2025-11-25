# Build stage
FROM golang:1.23-alpine AS builder

# Install build dependencies
# git: for go modules that reference git repositories
RUN apk add --no-cache git

# Set working directory
WORKDIR /app

# Copy go mod files first (better layer caching)
COPY go.mod go.sum* ./

# Download dependencies
# This layer is cached if go.mod and go.sum don't change
RUN go mod download

# Copy source code
COPY . .

# Build the application with optimizations
# CGO_ENABLED=0: Pure Go binary, no C dependencies (pgx v5 is pure Go)
# -ldflags '-w -s': Strip debug info and symbol table (smaller binary)
# -a: Force rebuild of all packages
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-w -s' -o consumer main.go

# Runtime stage - minimal image
FROM alpine:latest

# Install CA certificates for HTTPS connections
# Required for secure connections to Kafka and PostgreSQL
RUN apk --no-cache add ca-certificates

# Create non-root user for security
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /home/appuser

# Copy the binary from builder
COPY --from=builder /app/consumer .

# Change ownership to non-root user
RUN chown -R appuser:appgroup /home/appuser

# Switch to non-root user
USER appuser

# Health check (optional, but good practice)
# Checks if the process is still running
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD pgrep -f consumer || exit 1

# Run the consumer
CMD ["./consumer"]
