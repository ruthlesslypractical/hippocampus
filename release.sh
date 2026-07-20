#!/bin/bash
set -e

# release.sh — Package Hippocampus.app into a signed, notarized DMG
#
# Prerequisites:
#   - Run ./build.sh first (with CODESIGN_IDENTITY set)
#   - Apple Developer account with Developer ID Application certificate
#   - App-specific password stored in keychain (see below)
#
# Setup (one-time):
#   xcrun notarytool store-credentials "hippocampus-notary" \
#     --apple-id "your@email.com" \
#     --team-id "XXXXXXXXXX" \
#     --password "xxxx-xxxx-xxxx-xxxx"
#
# Usage:
#   CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)" ./release.sh
#
# Or if you've exported CODESIGN_IDENTITY in your shell profile, just:
#   ./release.sh

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
DIST_DIR="$ROOT_DIR/dist"
APP_DIR="$DIST_DIR/Hippocampus.app"
VERSION=$(grep 'const Version' internal/config/config.go | sed 's/.*"\(.*\)"/\1/')
DMG_NAME="Hippocampus-${VERSION}.dmg"
DMG_PATH="$DIST_DIR/$DMG_NAME"
SIGN_IDENTITY="${CODESIGN_IDENTITY:--}"
NOTARY_PROFILE="${NOTARY_KEYCHAIN_PROFILE:-hippocampus-notary}"

echo "=== Hippocampus Release v${VERSION} ==="
echo ""

# Verify the app exists
if [ ! -d "$APP_DIR" ]; then
    echo "❌ $APP_DIR not found. Run ./build.sh first."
    exit 1
fi

# Verify signing identity
if [ "$SIGN_IDENTITY" = "-" ]; then
    echo "⚠️  WARNING: No CODESIGN_IDENTITY set. DMG will be ad-hoc signed."
    echo "   Set CODESIGN_IDENTITY=\"Developer ID Application: ...\" for notarization."
    echo ""
    read -p "Continue with ad-hoc signing? (y/N) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

# Step 1: Create DMG staging area
echo "📦 Creating DMG..."
DMG_STAGING="$DIST_DIR/dmg-staging"
rm -rf "$DMG_STAGING"
mkdir -p "$DMG_STAGING"

cp -R "$APP_DIR" "$DMG_STAGING/"
ln -s /Applications "$DMG_STAGING/Applications"

# Remove any existing DMG
rm -f "$DMG_PATH"

# Create compressed DMG
hdiutil create -volname "Hippocampus" \
    -srcfolder "$DMG_STAGING" \
    -ov -format UDZO \
    "$DMG_PATH"

rm -rf "$DMG_STAGING"
echo "   → $DMG_PATH ($(du -h "$DMG_PATH" | cut -f1))"

# Step 2: Sign the DMG
echo ""
echo "🔏 Signing DMG..."
codesign --force --sign "$SIGN_IDENTITY" "$DMG_PATH"

# Step 3: Notarize (skip if ad-hoc)
if [ "$SIGN_IDENTITY" != "-" ]; then
    echo ""
    echo "📡 Submitting for notarization..."
    echo "   (Using keychain profile: $NOTARY_PROFILE)"
    echo "   This may take a few minutes..."
    echo ""

    xcrun notarytool submit "$DMG_PATH" \
        --keychain-profile "$NOTARY_PROFILE" \
        --wait

    echo ""
    echo "📎 Stapling notarization ticket..."
    xcrun stapler staple "$DMG_PATH"

    echo ""
    echo "✅ Verifying..."
    spctl --assess --verbose --type open "$DMG_PATH" 2>&1 || true
else
    echo ""
    echo "⏭️  Skipping notarization (ad-hoc signed)"
fi

echo ""
echo "=== Done! ==="
echo "   $DMG_PATH"
echo "   Version: $VERSION"
echo "   Size: $(du -h "$DMG_PATH" | cut -f1)"
echo ""
echo "Upload to GitHub Releases:"
echo "   gh release create v${VERSION} \"$DMG_PATH\" --title \"Hippocampus v${VERSION}\" --notes \"Initial release\""
