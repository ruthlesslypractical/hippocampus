#!/bin/sh
# acquire-redis.sh — Find or build redis-server
# Usage: acquire-redis.sh <BUILD_DIR> <REDIS_VERSION> <NCPU>
set -e

BUILD_DIR="$1"
REDIS_VERSION="$2"
NCPU="${3:-1}"
ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"

# Already have it?
[ -f "$BUILD_DIR/redis-server" ] && echo "  redis-server: cached" && exit 0

# Git submodule?
if [ -d "$ROOT_DIR/deps/redis" ]; then
    echo "  redis-server: building from source (deps/redis)..."
    cd "$ROOT_DIR/deps/redis"
    make -j"$NCPU" redis-server >/dev/null 2>&1
    cp src/redis-server "$BUILD_DIR/"
    exit 0
fi

# MacPorts?
if [ -f "/opt/local/bin/redis-server" ]; then
    echo "  redis-server: copying from MacPorts"
    cp /opt/local/bin/redis-server "$BUILD_DIR/"
    exit 0
fi

# Homebrew?
for path in /opt/homebrew/bin/redis-server /usr/local/bin/redis-server; do
    if [ -f "$path" ]; then
        echo "  redis-server: copying from Homebrew ($path)"
        cp "$path" "$BUILD_DIR/"
        exit 0
    fi
done

# FreeBSD pkg?
if [ -f "/usr/local/bin/redis-server" ]; then
    echo "  redis-server: copying from pkg"
    cp /usr/local/bin/redis-server "$BUILD_DIR/"
    exit 0
fi

# Download and compile
echo "  redis-server: downloading and compiling v${REDIS_VERSION}..."
TMPDIR=$(mktemp -d)
trap "rm -rf '$TMPDIR'" EXIT
curl -sL "https://github.com/redis/redis/archive/refs/tags/${REDIS_VERSION}.tar.gz" | tar xz -C "$TMPDIR"
cd "$TMPDIR/redis-${REDIS_VERSION}"
make -j"$NCPU" redis-server >/dev/null 2>&1
cp src/redis-server "$BUILD_DIR/"
echo "  redis-server: compiled"
