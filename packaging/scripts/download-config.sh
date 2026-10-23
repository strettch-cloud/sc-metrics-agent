#!/usr/bin/env bash
# SC-Metrics-Agent Config Download Script
# Purpose: Download the latest agent configuration from repository
# Usage: Called by post-install.sh and sc-metrics-agent-updater.sh
# Exit codes: 0=success, 1=download failed (non-critical), 2=critical error

set -euo pipefail

# === Variables ===
CONFIG_DIR="/etc/strettchcloud/config"
CONFIG_FILE="${CONFIG_DIR}/agent.yaml"
REPO_URL="${SC_CONFIG_REPO_URL:-https://repo.cloud.strettch.com/agent/config}"
LOG_PREFIX="[SC-Metrics-Agent-Config]"

# Logging function
log() {
    local level=$1
    shift
    local message="$*"
    local timestamp
    timestamp=$(date -u +"%Y-%m-%d %H:%M:%S UTC")

    case $level in
        ERROR|WARN)
            echo "${LOG_PREFIX} ${timestamp} [${level}] ${message}" >&2
            ;;
        *)
            echo "${LOG_PREFIX} ${timestamp} [${level}] ${message}"
            ;;
    esac
}

log "INFO" "Starting agent configuration download..."

# === Detect existing environment ===
ENVIRONMENT=""

if [ -f "${CONFIG_FILE}" ]; then
    log "INFO" "Detecting existing environment from ${CONFIG_FILE}..."
    # Try to extract environment from the existing config
    EXISTING_ENV=$(grep -E '^\s*environment:' "${CONFIG_FILE}" 2>/dev/null | head -n1 | awk '{print $2}' | tr -d '"' | tr -d "'" || echo "")

    if [ -n "${EXISTING_ENV}" ]; then
        ENVIRONMENT="${EXISTING_ENV}"
        log "INFO" "Detected existing environment: ${ENVIRONMENT}"
    fi
fi

# === Fallback to ENVIRONMENT variable or default ===
if [ -z "${ENVIRONMENT}" ]; then
    ENVIRONMENT="${SC_ENVIRONMENT:-production}"
    log "INFO" "No existing environment detected, using: ${ENVIRONMENT}"
fi

log "INFO" "Target environment: ${ENVIRONMENT}"

# === Ensure config directory exists ===
if ! mkdir -p "${CONFIG_DIR}" 2>/dev/null; then
    log "ERROR" "Failed to create config directory: ${CONFIG_DIR}"
    exit 2
fi

# === Create secure temporary file ===
TMP_FILE=$(mktemp /tmp/strettch-agent-config.XXXXXX)
trap 'rm -f "${TMP_FILE}"' EXIT INT TERM

# === Download config to temporary file ===
log "INFO" "Downloading agent configuration from ${REPO_URL}/${ENVIRONMENT}/agent.yaml"

# Use retry logic for robustness
MAX_RETRIES=3
RETRY_COUNT=0
DOWNLOAD_SUCCESS=false

while [ $RETRY_COUNT -lt $MAX_RETRIES ]; do
    if curl -fsSL --connect-timeout 30 --max-time 60 \
        "${REPO_URL}/${ENVIRONMENT}/agent.yaml" \
        -o "${TMP_FILE}" 2>&1; then

        # Verify file has content
        if [ -s "${TMP_FILE}" ]; then
            DOWNLOAD_SUCCESS=true
            log "INFO" "Configuration downloaded successfully"
            break
        else
            log "WARN" "Downloaded file is empty"
        fi
    fi

    RETRY_COUNT=$((RETRY_COUNT + 1))
    if [ $RETRY_COUNT -lt $MAX_RETRIES ]; then
        log "WARN" "Download failed, retrying ($RETRY_COUNT/$MAX_RETRIES)..."
        sleep 5
    else
        log "ERROR" "Failed to download configuration after ${MAX_RETRIES} attempts"
        log "WARN" "Agent will continue with existing configuration if available"
        exit 1
    fi
done

if [ "${DOWNLOAD_SUCCESS}" = true ]; then
    # === Move into place and set permissions ===
    if mv "${TMP_FILE}" "${CONFIG_FILE}"; then
        chmod 644 "${CONFIG_FILE}"
        log "INFO" "Configuration updated successfully: ${CONFIG_FILE}"
        log "INFO" "Environment: ${ENVIRONMENT}"
        exit 0
    else
        log "ERROR" "Failed to move config file to ${CONFIG_FILE}"
        exit 2
    fi
else
    log "ERROR" "Configuration download failed"
    exit 1
fi
