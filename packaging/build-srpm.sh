#!/bin/bash
# build-srpm.sh — Create a source RPM for Hippocampus
# Targets RHEL8/Rocky8 (works with mock or rpmbuild)
#
# Usage: ./packaging/build-srpm.sh [version]
#   Default version: read from internal/config/config.go
set -e

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SPEC="$ROOT_DIR/packaging/hippocampus.spec"

# Version
if [ -n "$1" ]; then
    VERSION="$1"
else
    VERSION=$(grep 'const Version' "$ROOT_DIR/internal/config/config.go" | sed 's/.*"\(.*\)"/\1/')
fi

if [ -z "$VERSION" ]; then
    echo "Error: could not determine version"
    exit 1
fi

echo "Building SRPM for hippocampus-${VERSION}..."

# Setup rpmbuild tree
RPMBUILD="$ROOT_DIR/rpmbuild"
mkdir -p "$RPMBUILD"/{SOURCES,SPECS,BUILD,RPMS,SRPMS}

# Create tarball (exclude .git, binaries, secrets)
TARBALL="$RPMBUILD/SOURCES/hippocampus-${VERSION}.tar.gz"
echo "  Creating source tarball..."
tar czf "$TARBALL" \
    --transform="s,^,hippocampus-${VERSION}/," \
    --exclude='.git' \
    --exclude='bin' \
    --exclude='dist' \
    --exclude='build' \
    --exclude='rpmbuild' \
    --exclude='*.dmg' \
    --exclude='*.pem' \
    --exclude='*.key' \
    --exclude='*.crt' \
    --exclude='config.json' \
    --exclude='.DS_Store' \
    --exclude='.ai' \
    -C "$ROOT_DIR" .

echo "  Tarball: $TARBALL ($(du -h "$TARBALL" | cut -f1))"

# Copy spec
cp "$SPEC" "$RPMBUILD/SPECS/"

# Patch version in spec if needed
sed -i "s/^%global version .*/%global version ${VERSION}/" "$RPMBUILD/SPECS/hippocampus.spec"

# Build SRPM
echo "  Building SRPM..."
rpmbuild --define "_topdir $RPMBUILD" \
    -bs "$RPMBUILD/SPECS/hippocampus.spec"

echo ""
echo "Done! SRPM:"
ls -la "$RPMBUILD/SRPMS/"*.src.rpm
echo ""
echo "To build the binary RPM:"
echo "  mock -r rocky-8-x86_64 $RPMBUILD/SRPMS/hippocampus-${VERSION}*.src.rpm"
echo ""
echo "Or with rpmbuild directly:"
echo "  rpmbuild --define '_topdir $RPMBUILD' -bb $RPMBUILD/SPECS/hippocampus.spec"
