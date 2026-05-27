#!/bin/bash
set -euo pipefail

PROJECT_ROOT="/Users/pablofernandez/Work/serial-killer"
cd "$PROJECT_ROOT"

echo "Building binaries..."
go build -o relay ./cmd/relay
go build -o client ./cmd/client

TMPDIR=$(mktemp -d)
trap 'echo "Cleaning up..."; kill %1 %2 2>/dev/null || true; wait; rm -rf "$TMPDIR"' EXIT

# Generate a test key using a temporary Go file
GENKEY_FILE="$TMPDIR/genkey.go"
cat > "$GENKEY_FILE" <<'GOEOF'
package main
import (
	"fmt"
	"fiatjaf.com/nostr"
)
func main() {
	fmt.Println(nostr.Generate().Hex())
}
GOEOF
TEST_KEY=$(go run "$GENKEY_FILE")

echo "Test key: $TEST_KEY"

# Start relay1
echo "Starting relay1 on port 3335..."
PORT=3335 DB_PATH="$TMPDIR/db1" STATE_PATH="$TMPDIR/chain1.json" ./relay > "$TMPDIR/relay1.log" 2>&1 &
sleep 1

# Start relay2
echo "Starting relay2 on port 3336..."
PORT=3336 DB_PATH="$TMPDIR/db2" STATE_PATH="$TMPDIR/chain2.json" ./relay > "$TMPDIR/relay2.log" 2>&1 &
sleep 1

# Run a single client session to demonstrate divergence and fix
echo "Running client session..."
./client > "$TMPDIR/session.log" 2>&1 <<COMMANDS
key $TEST_KEY
connect ws://localhost:3335
connect ws://localhost:3336
genesis test-chain "shared-genesis"
disconnect ws://localhost:3336
append test-chain "relay1-branch"
disconnect ws://localhost:3335
connect ws://localhost:3336
append test-chain "relay2-branch"
connect ws://localhost:3335
diverge test-chain
fix ws://localhost:3336 test-chain ws://localhost:3335
diverge test-chain
exit
COMMANDS

# Strip ANSI escape codes for easier grepping
CLEAN_LOG="$TMPDIR/session_clean.log"
sed 's/\x1b\[[0-9;]*m//g' "$TMPDIR/session.log" > "$CLEAN_LOG"

echo "Verifying outputs..."

# Check genesis published to both relays
if ! grep -q "to 2 relay" "$CLEAN_LOG"; then
	echo "ERROR: Genesis should be published to 2 relays"
	cat "$CLEAN_LOG"
	exit 1
fi

# Check divergence detected
if ! grep -q "DIVERGENCE" "$CLEAN_LOG"; then
	echo "ERROR: Expected divergence not detected"
	cat "$CLEAN_LOG"
	exit 1
fi

# Check fix operations
if ! grep -q "Deleting 1 event" "$CLEAN_LOG"; then
	echo "ERROR: Fix should delete 1 event"
	cat "$CLEAN_LOG"
	exit 1
fi

if ! grep -q "Replaying 1 event" "$CLEAN_LOG"; then
	echo "ERROR: Fix should replay 1 event"
	cat "$CLEAN_LOG"
	exit 1
fi

if ! grep -q "Replayed" "$CLEAN_LOG"; then
	echo "ERROR: Fix should replay events successfully"
	cat "$CLEAN_LOG"
	exit 1
fi

# Check final sync
if ! grep -q "are in sync" "$CLEAN_LOG"; then
	echo "ERROR: Relays should be in sync after fix"
	cat "$CLEAN_LOG"
	exit 1
fi

echo ""
echo "SUCCESS: All chain operations verified."
echo "  - Genesis created on both relays"
echo "  - Divergence created and detected"
echo "  - Divergence fixed automatically"
echo "  - Relays confirmed in sync"
