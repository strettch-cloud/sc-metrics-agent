#!/bin/bash
# SC-Agent Post-Installation Script
# Purpose: Configure directories, permissions, and start services after package installation
# Triggered by: APT package manager automatically after installing sc-metrics-agent.deb

set -euo pipefail

# Define service names used throughout the script
readonly SERVICE_NAME="sc-metrics-agent.service"
readonly UPDATER_SERVICE="sc-metrics-agent-update-scheduler.service"
readonly UPDATER_TIMER="sc-metrics-agent-update-scheduler.timer"

# Define directories
readonly RUNTIME_DIR="/var/run/sc-metrics-agent"
readonly CONFIG_DIR="/etc/sc-metrics-agent"
readonly STATE_DIR="/var/lib/sc-metrics-agent"
readonly CONFIG_DOWNLOAD_SCRIPT="/usr/lib/sc-metrics-agent/download-config.sh"

# Helper function for colored output
print_status() {
    local status=$1
    local message=$2
    case $status in
        "info") echo "ℹ️  $message" ;;
        "success") echo "✅ $message" ;;
        "warning") echo "⚠️  $message" ;;
        "error") echo "❌ $message" ;;
    esac
}

echo "Configuring SC-Metrics-Agent..."

# Refresh every APT keyring this host trusts for our repository: the path
# install.sh writes, plus whatever signed-by points at in any APT source for
# repo.cloud.strettch.com/metrics (older installers used other file names). Done
# first so a later failure in this script cannot leave the host unable to verify
# the next repository signing key.
readonly PACKAGED_KEYRING="/usr/lib/sc-metrics-agent/apt-keyring.gpg"
readonly REPO_URL_RE='repo\.cloud\.strettch\.com/metrics'

repo_keyrings() {
    echo "/usr/share/keyrings/sc-metrics-agent-keyring.gpg"
    # One-line sources: deb [signed-by=/a.gpg,/b.gpg] https://repo.cloud.strettch.com/metrics/...
    grep -hE "^[[:space:]]*deb[[:space:]].*${REPO_URL_RE}" /etc/apt/sources.list /etc/apt/sources.list.d/*.list 2>/dev/null \
        | grep -oE 'signed-by=[^] ]+' | cut -d= -f2 | tr ',' '\n' || true
    # deb822 sources: a Signed-By field in a stanza whose URIs name the repository.
    awk -v RS= -v re="$REPO_URL_RE" 'tolower($0) ~ /uris:/ && $0 ~ re' /etc/apt/sources.list.d/*.sources 2>/dev/null \
        | sed -nE 's/^[Ss]igned-[Bb]y:[[:space:]]*(\/[^[:space:]]+)$/\1/p' | tr ',' '\n' || true
}

if [ -f "$PACKAGED_KEYRING" ]; then
    repo_keyrings | sort -u | while read -r keyring; do
        case "$keyring" in
            *..*) print_status "warning" "Skipping APT keyring path with '..': $keyring" ;;
            /usr/share/keyrings/*.gpg|/etc/apt/keyrings/*.gpg)
                install -D -m 644 "$PACKAGED_KEYRING" "$keyring"
                print_status "info" "APT signing keyring updated: $keyring"
                ;;
            # A binary keyring written over an armored .asc, or anywhere else, would break APT.
            *) print_status "warning" "Not updating APT keyring outside the keyring directories: $keyring" ;;
        esac
    done
fi

# Create necessary directories with proper ownership
print_status "info" "Creating required directories..."
install -d -m 750 "$RUNTIME_DIR"
install -d -m 755 "$CONFIG_DIR"
install -d -m 750 "$STATE_DIR"

# Create default config if it doesn't exist
if [ ! -f "${CONFIG_DIR}/config.yaml" ]; then
    print_status "info" "Creating default configuration..."
    cat > "${CONFIG_DIR}/config.yaml" <<EOF
# SC-Metrics-Agent Configuration
EOF
    chmod 644 "${CONFIG_DIR}/config.yaml"
fi

# Download latest agent configuration from repository
print_status "info" "Downloading latest agent configuration..."
if [ -x "${CONFIG_DOWNLOAD_SCRIPT}" ]; then
    if ! "${CONFIG_DOWNLOAD_SCRIPT}"; then
        print_status "error" "Failed to download agent configuration"
        exit 1
    fi
    print_status "success" "Agent configuration downloaded successfully"
else
    print_status "error" "Config download script not found at ${CONFIG_DOWNLOAD_SCRIPT}"
    exit 1
fi

# Reload systemd to recognize new service files
print_status "info" "Reloading systemd configuration..."
if ! systemctl daemon-reload; then
    print_status "error" "Failed to reload systemd"
    exit 1
fi

# Function to safely enable service
enable_service() {
    local service=$1
    if ! systemctl is-enabled --quiet "$service" 2>/dev/null; then
        print_status "info" "Enabling $service..."
        if systemctl enable "$service" 2>/dev/null; then
            print_status "success" "$service enabled"
        else
            print_status "warning" "Could not enable $service"
            return 1
        fi
    else
        print_status "info" "$service is already enabled"
    fi
    return 0
}

# Enable services
enable_service "${SERVICE_NAME}"
enable_service "${UPDATER_TIMER}"

# Clear any previous failed states
systemctl reset-failed "${SERVICE_NAME}" 2>/dev/null || true
systemctl reset-failed "${UPDATER_SERVICE}" 2>/dev/null || true
systemctl reset-failed "${UPDATER_TIMER}" 2>/dev/null || true

# Function to start/restart service
start_or_restart_service() {
    local service=$1
    local is_timer=false
    
    [[ "$service" == *.timer ]] && is_timer=true
    
    if systemctl is-active --quiet "$service" 2>/dev/null; then
        print_status "info" "Restarting $service to apply updates..."
        if systemctl restart "$service"; then
            print_status "success" "$service restarted successfully"
        else
            print_status "warning" "$service restart failed"
            return 1
        fi
    else
        print_status "info" "Starting $service..."
        if systemctl start "$service"; then
            print_status "success" "$service started successfully"
        else
            print_status "warning" "$service could not be started"
            if [ "$is_timer" = false ]; then
                echo "  Please check 'systemctl status $service' for details"
                echo "  View logs with: journalctl -u $service -n 50"
            fi
            return 1
        fi
    fi
    return 0
}

# Start services (skip if auto-updater is handling this)
if [ "${SC_AGENT_AUTO_UPDATER:-0}" != "1" ]; then
    print_status "info" "Starting services..."
    start_or_restart_service "${SERVICE_NAME}"
    start_or_restart_service "${UPDATER_TIMER}"
else
    print_status "info" "Auto-updater detected, skipping automatic service start"
fi

# Wait a moment for services to stabilize (only if we started them)
if [ "${SC_AGENT_AUTO_UPDATER:-0}" != "1" ]; then
    sleep 2
fi

# Verify services are running (only if we started them)
if [ "${SC_AGENT_AUTO_UPDATER:-0}" != "1" ]; then
    # Allow some time for service to fully activate
    for i in {1..5}; do
        MAIN_STATUS=$(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || echo "inactive")
        if [ "$MAIN_STATUS" = "active" ] || [ "$MAIN_STATUS" = "activating" ]; then
            break
        fi
        sleep 1
    done
    
    if [ "$MAIN_STATUS" = "activating" ]; then
        MAIN_STATUS="active (starting)"
    fi
    
    TIMER_STATUS=$(systemctl is-active "${UPDATER_TIMER}" 2>/dev/null || echo "inactive")
else
    MAIN_STATUS="managed-by-updater"
    TIMER_STATUS="managed-by-updater"
fi

# Print status summary
echo ""
echo "╔════════════════════════════════════╗"
echo "║ SC-Metrics-Agent Installation Done ║"
echo "╚════════════════════════════════════╝"
echo ""
echo "Service Status:"
echo "  • Main Service: $MAIN_STATUS"
echo "  • Auto-Update:  $TIMER_STATUS"
echo ""
echo "Useful Commands:"
echo "  • Check status:   systemctl status sc-metrics-agent"
echo "  • View logs:      journalctl -u sc-metrics-agent -f"
echo "  • Configuration:  ${CONFIG_DIR}/config.yaml"
echo "  • Manual update:  systemctl start ${UPDATER_SERVICE}"
echo ""

# Exit with appropriate code
# Don't fail installation if service isn't active - it may not have a proper environment yet
# (e.g., during image build with Packer, no IaaS metadata available).
# The service is enabled and systemd will start it on boot.
if [ "${SC_AGENT_AUTO_UPDATER:-0}" = "1" ]; then
    exit 0
fi

# Log warning if services didn't start, but don't fail the installation
if [ "$MAIN_STATUS" != "active" ]; then
    print_status "warning" "Main service not active (may be normal during image build)"
fi

if [ "$TIMER_STATUS" != "active" ]; then
    print_status "warning" "Update timer not active (may be normal during image build)"
fi

exit 0
