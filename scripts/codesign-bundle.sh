#!/bin/sh
# codesign-bundle.sh — Sign all binaries in the app bundle
# Usage: codesign-bundle.sh <APP_DIR> <BUNDLE_ID_PREFIX> <SIGN_IDENTITY>
# Signs resources FIRST, main binary LAST (Apple requirement)
set -e

APP_DIR="$1"
BUNDLE_ID="$2"
SIGN_IDENTITY="${3:--}"

RES_DIR="$APP_DIR/Contents/Resources"

sign_binary() {
    local id="$1" bin="$2"
    [ -f "$bin" ] || return 0
    codesign --force --sign "$SIGN_IDENTITY" --options runtime --timestamp --identifier "$id" "$bin"
}

sign_lib() {
    [ -f "$1" ] || return 0
    codesign --force --sign "$SIGN_IDENTITY" --timestamp "$1"
}

# Sign our Go binaries (resources first)
sign_binary "$BUNDLE_ID.mcp-server"  "$RES_DIR/hippocampus-mcp"
sign_binary "$BUNDLE_ID.hook"        "$RES_DIR/hippocampus-hook"
sign_binary "$BUNDLE_ID.daemon"      "$RES_DIR/hippocampus-daemon"
sign_binary "$BUNDLE_ID.summarize"   "$RES_DIR/hippocampus-summarize"
sign_binary "$BUNDLE_ID.admin"       "$RES_DIR/hippocampus-admin"
sign_binary "$BUNDLE_ID.slack"       "$RES_DIR/hippocampus-slack"
sign_binary "$BUNDLE_ID.ingest"      "$RES_DIR/hippocampus-ingest"

# Sign bundled third-party binaries
sign_binary "$BUNDLE_ID.redis-server" "$RES_DIR/redis-server"
sign_binary "$BUNDLE_ID.redis-cli"    "$RES_DIR/redis-cli"
sign_binary "$BUNDLE_ID.ollama"       "$RES_DIR/ollama"

# Sign dylibs and modules
for lib in "$RES_DIR"/*.dylib "$RES_DIR"/*.so; do
    sign_lib "$lib"
done

# Sign main binary LAST (seals the bundle)
sign_binary "$BUNDLE_ID.app" "$APP_DIR/Contents/MacOS/Hippocampus"

echo "  Signed ($SIGN_IDENTITY)"
