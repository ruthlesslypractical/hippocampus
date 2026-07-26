#!/bin/sh
# acquire-redisearch.sh — Find redisearch.so
# Usage: acquire-redisearch.sh <BUILD_DIR>
set -e

BUILD_DIR="$1"
ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"

# Already have it?
[ -f "$BUILD_DIR/redisearch.so" ] && echo "  redisearch.so: cached" && exit 0

# MacPorts?
if [ -f "/opt/local/lib/redisearch.so" ]; then
    echo "  redisearch.so: copying from MacPorts"
    cp /opt/local/lib/redisearch.so "$BUILD_DIR/"
    exit 0
fi

# Homebrew redis-stack?
BREW_RS=$(find /opt/homebrew/lib /usr/local/lib -name "redisearch.so" 2>/dev/null | head -1)
if [ -n "$BREW_RS" ]; then
    echo "  redisearch.so: copying from Homebrew ($BREW_RS)"
    cp "$BREW_RS" "$BUILD_DIR/"
    exit 0
fi

# FreeBSD pkg?
if [ -f "/usr/local/lib/redisearch.so" ]; then
    echo "  redisearch.so: copying from pkg"
    cp /usr/local/lib/redisearch.so "$BUILD_DIR/"
    exit 0
fi

# Build from source (if submodule exists)
if [ -d "$ROOT_DIR/deps/redis/modules/redisearch" ]; then
    echo "  redisearch.so: building from source..."
    cd "$ROOT_DIR/deps/redis/modules/redisearch"
    make LTO=0 >/dev/null 2>&1
    RS_SO=$(find . -name "redisearch.so" -path "*/release/*" | head -1)
    if [ -n "$RS_SO" ]; then
        cp "$RS_SO" "$BUILD_DIR/"
        exit 0
    fi
fi

echo "  redisearch.so: not found (FT.SEARCH disabled, vector search still works)"
# Touch so Make doesn't re-run this every time
touch "$BUILD_DIR/redisearch.so.missing"
exit 0
