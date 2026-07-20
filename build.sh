#!/bin/bash
set -e

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_DIR="$ROOT_DIR/dist/Hippocampus.app"
BUILD_DIR="$ROOT_DIR/build"

REDIS_VERSION="8.8.0"

echo "Building Hippocampus.app..."
echo ""

# --- Redis acquisition strategy ---
# Priority:
#   1. Pre-built in app/build/ (fastest, for CI/releases)
#   2. Build from source via git submodule (auditable, for paranoid devs)
#   3. System install (MacPorts or Homebrew)
#   4. Download and compile (convenience default)

acquire_redis() {
    # Already have it?
    if [ -f "$BUILD_DIR/redis-server" ]; then
        echo "  Redis: using pre-built binary in app/build/"
        return 0
    fi

    # Git submodule?
    if [ -d "$ROOT_DIR/deps/redis" ]; then
        echo "  Redis: building from source (deps/redis)..."
        cd "$ROOT_DIR/deps/redis"
        make -j$(sysctl -n hw.ncpu 2>/dev/null || nproc) redis-server >/dev/null 2>&1
        cp src/redis-server "$BUILD_DIR/"
        return 0
    fi

    # System install? (MacPorts)
    if [ -f "/opt/local/bin/redis-server" ]; then
        echo "  Redis: using MacPorts install (/opt/local/bin/redis-server)"
        cp /opt/local/bin/redis-server "$BUILD_DIR/"
        return 0
    fi

    # System install? (Homebrew)
    if [ -f "/opt/homebrew/bin/redis-server" ] || [ -f "/usr/local/bin/redis-server" ]; then
        local REDIS_BIN=$(which redis-server 2>/dev/null || echo "/opt/homebrew/bin/redis-server")
        echo "  Redis: using Homebrew install ($REDIS_BIN)"
        cp "$REDIS_BIN" "$BUILD_DIR/"
        return 0
    fi

    # Download and compile
    echo "  Redis: downloading and compiling v${REDIS_VERSION}..."
    local TMPDIR=$(mktemp -d)
    curl -sL "https://github.com/redis/redis/archive/refs/tags/${REDIS_VERSION}.tar.gz" | tar xz -C "$TMPDIR"
    cd "$TMPDIR/redis-${REDIS_VERSION}"
    make -j$(sysctl -n hw.ncpu 2>/dev/null || nproc) redis-server >/dev/null 2>&1
    cp src/redis-server "$BUILD_DIR/"
    rm -rf "$TMPDIR"
    echo "  Redis: compiled successfully"
}

acquire_redisearch() {
    # Already have it?
    if [ -f "$BUILD_DIR/redisearch.so" ]; then
        echo "  RediSearch: using pre-built module in app/build/"
        return 0
    fi

    # System install? (MacPorts)
    if [ -f "/opt/local/lib/redisearch.so" ]; then
        echo "  RediSearch: using MacPorts install"
        cp /opt/local/lib/redisearch.so "$BUILD_DIR/"
        return 0
    fi

    # Homebrew redis-stack?
    local BREW_RS=$(find /opt/homebrew/lib /usr/local/lib -name "redisearch.so" 2>/dev/null | head -1)
    if [ -n "$BREW_RS" ]; then
        echo "  RediSearch: using Homebrew install ($BREW_RS)"
        cp "$BREW_RS" "$BUILD_DIR/"
        return 0
    fi

    # Build from redis source tree (if we downloaded it)
    if [ -d "$ROOT_DIR/deps/redis/modules/redisearch" ]; then
        echo "  RediSearch: building from source (deps/redis/modules/redisearch)..."
        cd "$ROOT_DIR/deps/redis/modules/redisearch"
        make LTO=0 >/dev/null 2>&1
        local RS_SO=$(find . -name "redisearch.so" -path "*/release/*" | head -1)
        if [ -n "$RS_SO" ]; then
            cp "$RS_SO" "$BUILD_DIR/"
            return 0
        fi
    fi

    echo "  RediSearch: not found (will work without it — FT.SEARCH disabled, vector search still works)"
    return 0
}

acquire_ollama() {
    # Already have it?
    if [ -f "$BUILD_DIR/ollama" ]; then
        echo "  Ollama: using pre-built binary in app/build/"
        return 0
    fi

    # System install? (MacPorts)
    if [ -f "/opt/local/bin/ollama" ]; then
        echo "  Ollama: using MacPorts install (/opt/local/bin/ollama)"
        cp /opt/local/bin/ollama "$BUILD_DIR/"
        return 0
    fi

    # Homebrew?
    if [ -f "/opt/homebrew/bin/ollama" ]; then
        echo "  Ollama: using Homebrew install (/opt/homebrew/bin/ollama)"
        cp /opt/homebrew/bin/ollama "$BUILD_DIR/"
        return 0
    fi

    # System PATH?
    local OLLAMA_BIN=$(which ollama 2>/dev/null)
    if [ -n "$OLLAMA_BIN" ]; then
        echo "  Ollama: using system install ($OLLAMA_BIN)"
        cp "$OLLAMA_BIN" "$BUILD_DIR/"
        return 0
    fi

    # Download from GitHub releases
    echo "  Ollama: downloading latest release..."
    local ARCH=$(uname -m)
    local OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    local DL_URL="https://github.com/ollama/ollama/releases/latest/download/ollama-${OS}"
    if [ "$ARCH" = "arm64" ] || [ "$ARCH" = "aarch64" ]; then
        DL_URL="${DL_URL}-arm64"
    else
        DL_URL="${DL_URL}-amd64"
    fi
    curl -sL "$DL_URL" -o "$BUILD_DIR/ollama"
    chmod +x "$BUILD_DIR/ollama"
    if [ -f "$BUILD_DIR/ollama" ] && [ -s "$BUILD_DIR/ollama" ]; then
        echo "  Ollama: downloaded successfully"
    else
        echo "  Ollama: download failed (summarization will be unavailable)"
        rm -f "$BUILD_DIR/ollama"
    fi
    return 0
}

# --- Build ---

mkdir -p "$BUILD_DIR"

echo "Acquiring dependencies..."
acquire_redis
acquire_redisearch
acquire_ollama
echo ""

echo "Compiling Go binaries..."
cd "$ROOT_DIR"
go build -o bin/hippocampus-mcp ./cmd/mcp-server/
go build -o bin/hippocampus-hook ./cmd/hook/
go build -o bin/hippocampus-summarize ./cmd/summarize/
go build -o bin/hippocampus-slack ./cmd/slack/
CGO_LDFLAGS="-framework UniformTypeIdentifiers" go build -tags production -o bin/hippocampus-app ./cmd/app/
echo ""

echo "Assembling app bundle..."
rm -rf "$APP_DIR"
mkdir -p "$APP_DIR/Contents/MacOS"
mkdir -p "$APP_DIR/Contents/Resources"

# Main app binary
cp bin/hippocampus-app "$APP_DIR/Contents/MacOS/Hippocampus"

# Bundled tools
cp bin/hippocampus-mcp "$APP_DIR/Contents/Resources/"
cp bin/hippocampus-hook "$APP_DIR/Contents/Resources/"
cp bin/hippocampus-summarize "$APP_DIR/Contents/Resources/"
cp bin/hippocampus-slack "$APP_DIR/Contents/Resources/"

# Redis (if available)
if [ -f "$BUILD_DIR/redis-server" ]; then
    cp "$BUILD_DIR/redis-server" "$APP_DIR/Contents/Resources/"
    # Rewrite OpenSSL dylib paths so redis-server finds them in the same directory
    if otool -L "$APP_DIR/Contents/Resources/redis-server" | grep -q "/opt/local"; then
        install_name_tool -change \
            /opt/local/libexec/openssl3/lib/libssl.3.dylib \
            @loader_path/libssl.3.dylib \
            "$APP_DIR/Contents/Resources/redis-server"
        install_name_tool -change \
            /opt/local/libexec/openssl3/lib/libcrypto.3.dylib \
            @loader_path/libcrypto.3.dylib \
            "$APP_DIR/Contents/Resources/redis-server"
    fi
fi
if [ -f "$BUILD_DIR/redis-cli" ]; then
    cp "$BUILD_DIR/redis-cli" "$APP_DIR/Contents/Resources/"
fi
if [ -f "$BUILD_DIR/redisearch.so" ]; then
    cp "$BUILD_DIR/redisearch.so" "$APP_DIR/Contents/Resources/"
    # Bundle OpenSSL dylibs that redisearch depends on
    [ -f "$BUILD_DIR/libssl.3.dylib" ] && cp "$BUILD_DIR/libssl.3.dylib" "$APP_DIR/Contents/Resources/"
    [ -f "$BUILD_DIR/libcrypto.3.dylib" ] && cp "$BUILD_DIR/libcrypto.3.dylib" "$APP_DIR/Contents/Resources/"
    # Rewrite dylib paths in redisearch.so
    if otool -L "$APP_DIR/Contents/Resources/redisearch.so" | grep -q "/opt/local"; then
        install_name_tool -change \
            /opt/local/libexec/openssl3/lib/libssl.3.dylib \
            @loader_path/libssl.3.dylib \
            "$APP_DIR/Contents/Resources/redisearch.so"
        install_name_tool -change \
            /opt/local/libexec/openssl3/lib/libcrypto.3.dylib \
            @loader_path/libcrypto.3.dylib \
            "$APP_DIR/Contents/Resources/redisearch.so"
    fi
    # Fix dylib cross-references (libssl references libcrypto)
    if [ -f "$APP_DIR/Contents/Resources/libssl.3.dylib" ]; then
        install_name_tool -change \
            /opt/local/libexec/openssl3/lib/libcrypto.3.dylib \
            @loader_path/libcrypto.3.dylib \
            "$APP_DIR/Contents/Resources/libssl.3.dylib"
    fi
fi

# Ollama (if available)
if [ -f "$BUILD_DIR/ollama" ]; then
    cp "$BUILD_DIR/ollama" "$APP_DIR/Contents/Resources/"
fi

# Resources
cp "$BUILD_DIR/appicon.icns" "$APP_DIR/Contents/Resources/" 2>/dev/null || true
cp "$BUILD_DIR/Info.plist" "$APP_DIR/Contents/"

# LaunchAgent templates
mkdir -p "$APP_DIR/Contents/Resources/launchd"
cp "$BUILD_DIR/launchd/"*.plist "$APP_DIR/Contents/Resources/launchd/" 2>/dev/null || true

# Permissions
chmod +x "$APP_DIR/Contents/MacOS/Hippocampus"
chmod +x "$APP_DIR/Contents/Resources/"hippocampus-* 2>/dev/null || true
chmod +x "$APP_DIR/Contents/Resources/redis-server" 2>/dev/null || true
chmod +x "$APP_DIR/Contents/Resources/ollama" 2>/dev/null || true

# Code signing — proper identifiers so macOS Local Network privacy and
# Little Snitch can track each binary independently across rebuilds.
echo "Signing binaries..."
BUNDLE_ID_PREFIX="com.ruthlesslypractical.hippocampus"
SIGN_IDENTITY="${CODESIGN_IDENTITY:--}"  # Use ad-hoc (-) unless CODESIGN_IDENTITY is set

sign_binary() {
    codesign --force --sign "$SIGN_IDENTITY" --options runtime --timestamp --identifier "$1" "$2"
}

sign_lib() {
    codesign --force --sign "$SIGN_IDENTITY" --timestamp "$1"
}

# Sign all our Go binaries (hardened runtime + secure timestamp required for notarization)
# IMPORTANT: Sign resources FIRST, main binary LAST (signing resources invalidates the app seal)
sign_binary "$BUNDLE_ID_PREFIX.mcp-server" "$APP_DIR/Contents/Resources/hippocampus-mcp"
sign_binary "$BUNDLE_ID_PREFIX.hook" "$APP_DIR/Contents/Resources/hippocampus-hook"
sign_binary "$BUNDLE_ID_PREFIX.summarize" "$APP_DIR/Contents/Resources/hippocampus-summarize"
sign_binary "$BUNDLE_ID_PREFIX.slack" "$APP_DIR/Contents/Resources/hippocampus-slack"

# Sign bundled third-party binaries
[ -f "$APP_DIR/Contents/Resources/redis-server" ] && \
    sign_binary "$BUNDLE_ID_PREFIX.redis-server" "$APP_DIR/Contents/Resources/redis-server"
[ -f "$APP_DIR/Contents/Resources/redis-cli" ] && \
    sign_binary "$BUNDLE_ID_PREFIX.redis-cli" "$APP_DIR/Contents/Resources/redis-cli"
[ -f "$APP_DIR/Contents/Resources/ollama" ] && \
    sign_binary "$BUNDLE_ID_PREFIX.ollama" "$APP_DIR/Contents/Resources/ollama"

# Sign dylibs and modules (timestamp but no hardened runtime for non-executables)
for dylib in "$APP_DIR/Contents/Resources/"*.dylib "$APP_DIR/Contents/Resources/"*.so; do
    [ -f "$dylib" ] && sign_lib "$dylib" 2>/dev/null || true
done

# Sign the main binary LAST (seals the entire bundle)
sign_binary "$BUNDLE_ID_PREFIX.app" "$APP_DIR/Contents/MacOS/Hippocampus"

echo ""
echo "Done! → dist/Hippocampus.app"
echo "  Size: $(du -sh "$APP_DIR" | cut -f1)"
echo ""
echo "Contents:"
ls -lh "$APP_DIR/Contents/Resources/" | grep -v "^total" | awk '{print "  " $NF " (" $5 ")"}'
echo ""
echo "To install: cp -r dist/Hippocampus.app /Applications/"
echo ""
echo "Build options:"
echo "  • To build Redis from source: git submodule add https://github.com/redis/redis deps/redis"
echo "  • To use system Redis: install via 'sudo port install redis' or 'brew install redis'"
echo "  • Pre-built binaries: place redis-server and redisearch.so in app/build/"
