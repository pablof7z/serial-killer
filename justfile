set shell := ["bash", "-c"]

default:
    @just --list

# Build the client binary
build-client:
    go build -o client ./cmd/client

# Build the relay binary
build-relay:
    go build -o relay ./cmd/relay

# Build both binaries
build-all: build-client build-relay

# Build everything
build: build-all

# Run a single relay (default config)
run-relay:
    go run ./cmd/relay

# Run the client
run-client:
    go run ./cmd/client

# Run two relays on separate ports with separate data directories
run-relays: build-relay
    echo "Starting relay A on port 3334..."
    ./relay -port 3334 -data ./relay-a & echo $! > .relay-a.pid
    echo "Starting relay B on port 3335..."
    ./relay -port 3335 -data ./relay-b & echo $! > .relay-b.pid
    echo "Relays running:"
    echo "  A: ws://localhost:3334 (data: ./relay-a)"
    echo "  B: ws://localhost:3335 (data: ./relay-b)"
    echo "Run 'just stop-relays' to shut them down."

# Stop the running relays
stop-relays:
    -test -f .relay-a.pid && kill $(cat .relay-a.pid) 2>/dev/null && rm -f .relay-a.pid
    -test -f .relay-b.pid && kill $(cat .relay-b.pid) 2>/dev/null && rm -f .relay-b.pid
    echo "Relays stopped."

# Clean relay data directories
clean-relays: stop-relays
    rm -rf relay-a relay-b

# Clean build artifacts
clean:
    rm -f client relay .relay-a.pid .relay-b.pid
    rm -rf relay-a relay-b

# Tidy Go dependencies
tidy:
    go mod tidy

# Run tests
test:
    go test ./...

# Run tests verbosely
test-v:
    go test -v ./...

# Format all Go files
fmt:
    go fmt ./...

# Run vet on all packages
vet:
    go vet ./...

# Full check: format, vet, test
check: fmt vet test
