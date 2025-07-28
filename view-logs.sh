#!/bin/bash
# Script to view logs from both host and container

echo "=== CONTAINER-USE LOGS ==="
echo ""

# Find the latest container
CONTAINER=$(docker ps --filter "name=cu-" --format "{{.Names}}" | head -1)

if [ -z "$CONTAINER" ]; then
    echo "No running container found"
    exit 1
fi

echo "Container: $CONTAINER"
echo ""

echo "=== PROXY LOG (in container) ==="
docker exec $CONTAINER cat /tmp/container-use-proxy.log 2>/dev/null || echo "No proxy log found"
echo ""

echo "=== INJECT LOG (tool tracking) ==="
docker exec $CONTAINER tail -20 /tmp/inject.log 2>/dev/null || echo "No inject log found"
echo ""

echo "=== CLAUDE PROCESSES ==="
docker exec $CONTAINER ps aux | grep -E "claude|proxy" | grep -v grep
echo ""

echo "=== ENVIRONMENT ==="
docker exec $CONTAINER sh -c 'echo "ANTHROPIC_BASE_URL=$ANTHROPIC_BASE_URL"'
docker exec $CONTAINER sh -c 'echo "MANAGER_ADDR=$MANAGER_ADDR"'
echo ""

echo "=== CREDENTIALS CHECK ==="
docker exec $CONTAINER sh -c 'ls -la /home/cosmos/.claude/.credentials.json 2>/dev/null && echo "Credentials file exists" || echo "No credentials file"'
docker exec $CONTAINER sh -c 'test -s /home/cosmos/.claude/.credentials.json && echo "Credentials file has content" || echo "Credentials file is empty"'

# Check host-side manager logs if available
echo ""
echo "=== HOST MANAGER STATUS ==="
ps aux | grep container-use | grep -v grep | head -5