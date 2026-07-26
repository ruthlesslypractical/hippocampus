#!/bin/sh
# acquire-ollama.sh — Find or download ollama
# Usage: acquire-ollama.sh <BUILD_DIR> <OS> <ARCH>
set -e

BUILD_DIR="$1"
OS_NAME="$2"
ARCH="$3"

# Already have it?
[ -f "$BUILD_DIR/ollama" ] && echo "  ollama: cached" && exit 0

# MacPorts?
if [ -f "/opt/local/bin/ollama" ]; then
    echo "  ollama: copying from MacPorts"
    cp /opt/local/bin/ollama "$BUILD_DIR/"
    exit 0
fi

# Homebrew?
if [ -f "/opt/homebrew/bin/ollama" ]; then
    echo "  ollama: copying from Homebrew"
    cp /opt/homebrew/bin/ollama "$BUILD_DIR/"
    exit 0
fi

# System PATH?
OLLAMA_BIN=$(which ollama 2>/dev/null || true)
if [ -n "$OLLAMA_BIN" ]; then
    echo "  ollama: copying from system ($OLLAMA_BIN)"
    cp "$OLLAMA_BIN" "$BUILD_DIR/"
    exit 0
fi

# Download from GitHub
echo "  ollama: downloading latest release..."
DL_OS=$(echo "$OS_NAME" | tr '[:upper:]' '[:lower:]')
case "$ARCH" in
    arm64|aarch64) DL_ARCH="arm64" ;;
    x86_64|amd64)  DL_ARCH="amd64" ;;
    *)             DL_ARCH="amd64" ;;
esac

DL_URL="https://github.com/ollama/ollama/releases/latest/download/ollama-${DL_OS}-${DL_ARCH}"
curl -sL "$DL_URL" -o "$BUILD_DIR/ollama"
chmod +x "$BUILD_DIR/ollama"

if [ -f "$BUILD_DIR/ollama" ] && [ -s "$BUILD_DIR/ollama" ]; then
    echo "  ollama: downloaded"
else
    echo "  ollama: download failed (summarization unavailable)"
    rm -f "$BUILD_DIR/ollama"
fi
exit 0
