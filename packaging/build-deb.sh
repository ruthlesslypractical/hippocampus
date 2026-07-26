#!/bin/bash
# build-deb.sh — Create a .deb package for Hippocampus
# Target: Ubuntu 25.10 (Oracular Oriole) amd64
#
# Usage: ./packaging/build-deb.sh [version]
#   Default version: read from internal/config/config.go
#
# Prerequisites (on build host):
#   apt install debhelper devscripts golang-go git
#
# For cross-compilation or clean-room builds:
#   apt install sbuild schroot debootstrap
#   sbuild-createchroot oracular /srv/chroot/oracular-amd64 http://archive.ubuntu.com/ubuntu
set -e

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"

# Version
if [ -n "$1" ]; then
    VERSION="$1"
else
    VERSION=$(grep 'var Version' "$ROOT_DIR/internal/config/config.go" | sed 's/.*"\(.*\)"/\1/')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "0.0.0-dev" ]; then
    echo "Error: could not determine version (got: '$VERSION')"
    echo "Set version via argument or ensure internal/config/config.go has a release version"
    exit 1
fi

echo "Building .deb for hippocampus ${VERSION} (Ubuntu 25.10 / oracular)..."

# Build directory
BUILD_DIR="$ROOT_DIR/debbuild"
rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR/hippocampus-${VERSION}"

# Copy source (exclude junk)
echo "  Copying source..."
rsync -a --exclude='.git' --exclude='bin' --exclude='dist' --exclude='build' \
    --exclude='rpmbuild' --exclude='debbuild' --exclude='*.dmg' \
    --exclude='*.pem' --exclude='*.key' --exclude='*.crt' \
    --exclude='config.json' --exclude='.DS_Store' --exclude='.ai' \
    "$ROOT_DIR/" "$BUILD_DIR/hippocampus-${VERSION}/"

# Copy debian/ into source tree
cp -r "$ROOT_DIR/packaging/debian" "$BUILD_DIR/hippocampus-${VERSION}/debian"

# Patch version in changelog
sed -i "s/^hippocampus (.*)/hippocampus (${VERSION}-1) oracular; urgency=medium/" \
    "$BUILD_DIR/hippocampus-${VERSION}/debian/changelog"

cd "$BUILD_DIR/hippocampus-${VERSION}"

# Build the package
echo "  Running dpkg-buildpackage..."
dpkg-buildpackage -us -uc -b

echo ""
echo "Done! Package:"
ls -la "$BUILD_DIR"/hippocampus_*.deb
echo ""
echo "Install with:"
echo "  sudo dpkg -i $BUILD_DIR/hippocampus_${VERSION}-1_amd64.deb"
echo "  sudo apt-get install -f  # resolve dependencies"
echo ""
echo "Or for a clean-room build with sbuild:"
echo "  sbuild --dist=oracular --arch=amd64 $BUILD_DIR/hippocampus-${VERSION}"
