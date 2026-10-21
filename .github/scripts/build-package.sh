#!/bin/bash
# scripts/build-package.sh - Package building helper
set -euo pipefail

VERSION="${1:?Version required}"
CHANNEL="${2:-stable}"
ARCH="${3:-amd64}"
PACKAGE_NAME="sc-metrics-agent"

echo "Building package: ${PACKAGE_NAME} v${VERSION} (${CHANNEL})"

# Create staging directory
STAGING_DIR="/tmp/${PACKAGE_NAME}-staging"
rm -rf "$STAGING_DIR"
mkdir -p "${STAGING_DIR}/usr/bin" "${STAGING_DIR}/etc/systemd/system" "${STAGING_DIR}/usr/lib/${PACKAGE_NAME}"

# Copy binaries
cp "build/${PACKAGE_NAME}" "${STAGING_DIR}/usr/bin/"
chmod +x "${STAGING_DIR}/usr/bin/${PACKAGE_NAME}"

# Copy updater script
cp "packaging/scripts/${PACKAGE_NAME}-updater.sh" "${STAGING_DIR}/usr/bin/"
chmod +x "${STAGING_DIR}/usr/bin/${PACKAGE_NAME}-updater.sh"

# Copy config download script
cp "packaging/scripts/download-config.sh" "${STAGING_DIR}/usr/lib/${PACKAGE_NAME}/"
chmod +x "${STAGING_DIR}/usr/lib/${PACKAGE_NAME}/download-config.sh"

# Copy systemd files
cp "packaging/systemd/${PACKAGE_NAME}.service" "${STAGING_DIR}/etc/systemd/system/"
cp "packaging/systemd/${PACKAGE_NAME}-update-scheduler.service" "${STAGING_DIR}/etc/systemd/system/"
cp "packaging/systemd/${PACKAGE_NAME}-update-scheduler.timer" "${STAGING_DIR}/etc/systemd/system/"

# APT keyring: post-install copies it over the one the installer at
# repo.cloud.strettch.com/metrics/install.sh downloaded, so each
# upgrade refreshes which repository signing keys the host trusts.
./.github/scripts/build-keyring.sh "${STAGING_DIR}/usr/lib/${PACKAGE_NAME}/apt-keyring.gpg"

# License terms travel with the binary.
install -D -m 644 LICENSE "${STAGING_DIR}/usr/share/doc/${PACKAGE_NAME}/LICENSE"
install -D -m 644 NOTICE "${STAGING_DIR}/usr/share/doc/${PACKAGE_NAME}/NOTICE"

# Build .deb package
DESCRIPTION="SC Metrics Agent - System monitoring and metrics collection"
[ "$CHANNEL" = "beta" ] && DESCRIPTION="$DESCRIPTION (Beta)"

fpm -s dir -t deb \
    -n "$PACKAGE_NAME" \
    -v "$VERSION" \
    -a "$ARCH" \
    -C "$STAGING_DIR/" \
    --description "$DESCRIPTION" \
    --maintainer "engineering@strettch.com" \
    --url "https://github.com/strettch-cloud/sc-metrics-agent" \
    --depends "libc6" \
    --depends "dmidecode" \
    --depends "jq" \
    --depends "curl" \
    --after-install "packaging/scripts/post-install.sh" \
    --before-remove "packaging/scripts/pre-remove.sh" \
    --log error

echo "Package built: ${PACKAGE_NAME}_${VERSION}_${ARCH}.deb"