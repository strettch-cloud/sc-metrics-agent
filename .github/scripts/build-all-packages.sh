#!/bin/bash
# scripts/build-all-packages.sh - Build multiple package formats
set -euo pipefail

VERSION="${1:?Version required}"
CHANNEL="${2:-stable}"

# Build for multiple architectures and formats
for arch in amd64 arm64; do
    echo "Building for $arch..."
    
    # Build binary for architecture
    GOOS=linux GOARCH=$arch VERSION="$VERSION" mise run build
    
    # Build DEB
    ./.github/scripts/build-package.sh "$VERSION" "$CHANNEL" "$arch"
    
    # Rename binary for this architecture and move to build directory
    cp build/sc-metrics-agent "build/sc-metrics-agent-linux-$arch"

done

# Generate checksums
sha256sum -- *.deb > packages.sha256

echo "All packages built successfully"