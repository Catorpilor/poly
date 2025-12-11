# Build stage
# Note: golang:1.24-alpine supports multi-arch including ARM64 (Raspberry Pi)
FROM golang:1.24-alpine AS builder

# Install build dependencies
RUN apk add --no-cache gcc musl-dev

WORKDIR /app

# Copy go mod files first for better caching
# COPY go.mod go.sum ./
# RUN go mod download

# Copy source code
COPY . .

# Build the binary with static linking
# GOARCH is auto-detected based on the build platform
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /app/polybot ./cmd/bot

# Final stage
FROM alpine:3.21

# Install ca-certificates for HTTPS requests
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user for security
RUN adduser -D -g '' appuser

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/polybot .

# Use non-root user
USER appuser

# Health check (optional - bot doesn't expose HTTP but useful for debugging)
# HEALTHCHECK --interval=30s --timeout=3s CMD pgrep polybot || exit 1

ENTRYPOINT ["/app/polybot"]
