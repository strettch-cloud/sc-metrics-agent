#!/bin/bash
# SC-Agent Installation Script
# Purpose: Add package repository and install sc-metrics-agent via APT
# Usage: curl -sSL https://repo.cloud.strettch.com/metrics/install.sh | sudo bash

set -euo pipefail 

# Repository configuration 
PACKAGE_REPO_URL="${SC_REPO_URL:-https://repo.cloud.strettch.com/metrics}"
PACKAGE_NAME="sc-metrics-agent"
APT_LOCK_TIMEOUT="${SC_APT_LOCK_TIMEOUT:-300}"
APT_MAX_RETRIES="${SC_APT_MAX_RETRIES:-5}"
APT_RETRY_DELAY="${SC_APT_RETRY_DELAY:-15}"

# Add trap for cleanup on failure
cleanup() {
    if [ $? -ne 0 ]; then
        echo "Installation failed. Cleaning up..."
        # Remove partial configurations if they exist
        rm -f "/etc/apt/sources.list.d/${PACKAGE_NAME}.list" 2>/dev/null || true
        rm -f "/usr/share/keyrings/${PACKAGE_NAME}-keyring.gpg" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Check if running as root
if [ "$EUID" -ne 0 ]; then 
    echo "Error: This installer must be run as root (use sudo)"
    exit 1
fi

# Check if running on Debian/Ubuntu (only supported systems)
if [ ! -f /etc/debian_version ]; then
    echo "Error: This installer only supports Debian/Ubuntu systems"
    echo "Detected OS does not have /etc/debian_version"
    exit 1
fi

# Detect specific distribution and version for better error messages
if [ -f /etc/os-release ]; then
    # Only exists on the machine being installed on, so shellcheck cannot read it.
    # shellcheck source=/dev/null
    . /etc/os-release
    echo "Detected OS: ${NAME} ${VERSION_ID}"
fi

echo "Installing SC-Metrics-Agent on Debian/Ubuntu system..."

# Run apt-get commands with lock timeout and retry to avoid transient dpkg contention
apt_get_with_retry() {
    local attempt=1

    while [ "${attempt}" -le "${APT_MAX_RETRIES}" ]; do
        if apt-get -o DPkg::Lock::Timeout="${APT_LOCK_TIMEOUT}" "$@"; then
            return 0
        fi

        if [ "${attempt}" -lt "${APT_MAX_RETRIES}" ]; then
            echo "APT command failed (attempt ${attempt}/${APT_MAX_RETRIES}), retrying in ${APT_RETRY_DELAY}s..."
            sleep "${APT_RETRY_DELAY}"
        fi

        attempt=$((attempt + 1))
    done

    return 1
}

# Check for required tools
for tool in curl gpg apt-get systemctl; do
    if ! command -v "$tool" &> /dev/null; then
        echo "Error: Required tool '$tool' is not installed"
        exit 1
    fi
done

# Create keyrings directory if it doesn't exist (for older systems)
mkdir -p /usr/share/keyrings

# Download and install GPG key for package verification with retry logic
echo "Adding repository GPG key..."
MAX_RETRIES=3
RETRY_COUNT=0
while [ $RETRY_COUNT -lt $MAX_RETRIES ]; do
    if curl -fsSL --connect-timeout 30 --max-time 60 "${PACKAGE_REPO_URL}/gpg.key" | \
       gpg --dearmor | tee /usr/share/keyrings/${PACKAGE_NAME}-keyring.gpg > /dev/null; then
        break
    else
        RETRY_COUNT=$((RETRY_COUNT + 1))
        if [ $RETRY_COUNT -lt $MAX_RETRIES ]; then
            echo "Failed to download GPG key, retrying ($RETRY_COUNT/$MAX_RETRIES)..."
            sleep 5
        else
            echo "Error: Failed to download GPG key after $MAX_RETRIES attempts"
            exit 1
        fi
    fi
done

# Verify GPG key was created and has content
if [ ! -s "/usr/share/keyrings/${PACKAGE_NAME}-keyring.gpg" ]; then
    echo "Error: GPG keyring file is empty or missing"
    exit 1
fi

# Add repository to APT sources
echo "Adding repository to APT sources..."
cat > "/etc/apt/sources.list.d/${PACKAGE_NAME}.list" <<EOF
deb [signed-by=/usr/share/keyrings/${PACKAGE_NAME}-keyring.gpg] ${PACKAGE_REPO_URL}/apt stable main
EOF

# Update package index — scoped to only our source so unrelated third-party
# repos on the host cannot block install by failing `apt-get update`.
echo "Updating package index..."
if ! apt_get_with_retry update \
    -o Dir::Etc::sourcelist="sources.list.d/${PACKAGE_NAME}.list" \
    -o Dir::Etc::sourceparts="/dev/null" \
    -o APT::Get::List-Cleanup="0"; then
    echo "Error: Failed to update package index"
    echo "This might be a temporary network issue. Please try again later."
    exit 1
fi

# Install the agent package with lock wait
echo "Installing ${PACKAGE_NAME} package..."
if ! apt_get_with_retry install -y \
    -o Dpkg::Options::="--force-confdef" \
    -o Dpkg::Options::="--force-confold" \
    ${PACKAGE_NAME}; then
    echo "Error: Failed to install ${PACKAGE_NAME}"
    echo "Check /var/log/apt/history.log for details"
    exit 1
fi

echo "SC-Metrics-Agent installed successfully"
echo "Run 'systemctl status ${PACKAGE_NAME}' to check service status"

# Clear the trap
trap - EXIT