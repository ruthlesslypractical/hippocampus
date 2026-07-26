#!/bin/sh
# fix-dylib-paths.sh — Rewrite @loader_path references in bundled binaries
# Usage: fix-dylib-paths.sh <RESOURCES_DIR>
set -e

RES_DIR="$1"

# Only applies to macOS bundles
[ "$(uname -s)" = "Darwin" ] || exit 0

fix_binary() {
    local bin="$1"
    [ -f "$bin" ] || return 0
    
    if otool -L "$bin" 2>/dev/null | grep -q "/opt/local"; then
        install_name_tool -change \
            /opt/local/libexec/openssl3/lib/libssl.3.dylib \
            @loader_path/libssl.3.dylib \
            "$bin" 2>/dev/null || true
        install_name_tool -change \
            /opt/local/libexec/openssl3/lib/libcrypto.3.dylib \
            @loader_path/libcrypto.3.dylib \
            "$bin" 2>/dev/null || true
    fi
}

# Fix redis-server
fix_binary "$RES_DIR/redis-server"

# Fix redisearch.so
fix_binary "$RES_DIR/redisearch.so"

# Fix libssl → libcrypto cross-reference
if [ -f "$RES_DIR/libssl.3.dylib" ]; then
    install_name_tool -change \
        /opt/local/libexec/openssl3/lib/libcrypto.3.dylib \
        @loader_path/libcrypto.3.dylib \
        "$RES_DIR/libssl.3.dylib" 2>/dev/null || true
fi
