#!/bin/bash
# SC-Agent Auto-Update Script
# Purpose: Check for and install updates ONLY for sc-metrics-agent package
# Triggered by: sc-metrics-agent-update-scheduler.timer

set -euo pipefail

# Configuration
readonly PACKAGE_NAME="sc-metrics-agent"
readonly LOG_PREFIX="[SC-Metrics-Agent-Updater]"
readonly LOCK_FILE="/var/lock/sc-metrics-agent-updater.lock"
readonly CONFIG_FILE="/etc/${PACKAGE_NAME}/config.yaml"
readonly MAX_RETRIES=3
readonly RETRY_DELAY=30
readonly APT_LOCK_TIMEOUT="${SC_APT_LOCK_TIMEOUT:-300}"
readonly CONFIG_DOWNLOAD_SCRIPT="/usr/lib/sc-metrics-agent/download-config.sh"
# The agent's APT source: one line, as install.sh writes it, or deb822.
readonly LIST_SOURCE="/etc/apt/sources.list.d/${PACKAGE_NAME}.list"
readonly DEB822_SOURCE="/etc/apt/sources.list.d/${PACKAGE_NAME}.sources"
# What an Ubuntu release upgrade leaves of a one-line source it cannot convert:
# a commented copy (upgrades to 26.04 and later), or only its own backup of
# the file (the upgrade to 24.04, which removes the source itself).
readonly DISABLED_LIST_SOURCE="${LIST_SOURCE}.disabled"
readonly BACKUP_LIST_SOURCE="${LIST_SOURCE}.distUpgrade"
readonly PACKAGED_KEYRING="/usr/lib/${PACKAGE_NAME}/apt-keyring.gpg"
# The edition install.sh gives a system the repository does not publish.
readonly FALLBACK_SUITE="noble"
SOURCE_CANDIDATE=""
SOURCE_ERROR=""

# Ensure we're running as root
if [ "$EUID" -ne 0 ]; then 
    echo "${LOG_PREFIX} Error: Must run as root" >&2
    exit 1
fi

# Logging function
log() {
    local level=$1
    shift
    local message="$*"
    local timestamp
    timestamp=$(date -u +"%Y-%m-%d %H:%M:%S UTC")
    
    # Use appropriate output stream
    case $level in
        ERROR|WARN)
            echo "${LOG_PREFIX} ${timestamp} [${level}] ${message}" >&2
            ;;
        *)
            echo "${LOG_PREFIX} ${timestamp} [${level}] ${message}"
            ;;
    esac
}

# Cleanup function (preserving original atomic lock approach)
# shellcheck disable=SC2329 # called from the trap below, which shellcheck does not follow
cleanup() {
    rmdir "${LOCK_FILE}" 2>/dev/null || true
    rm -f "${SOURCE_CANDIDATE}"
}

# Set up exit trap
trap cleanup EXIT INT TERM

# Check if another instance is running (using atomic mkdir approach from original)
if ! mkdir "${LOCK_FILE}" 2>/dev/null; then
    log "WARN" "Another updater instance is running"
    exit 0
fi

# Start update process
log "INFO" "Starting update check for ${PACKAGE_NAME}"

# Check OS compatibility
if [ ! -f /etc/debian_version ]; then
    log "ERROR" "Not a Debian/Ubuntu system"
    exit 1
fi

# Ubuntu's release upgrade (do-release-upgrade) unpacks its upgrader to
# /tmp/ubuntu-release-upgrader-<random>/ and runs it from there, as root. While
# it runs it rebuilds APT's caches, and an `apt-get update` at the wrong moment
# removes the files it is writing and makes it abort before its clean-up.
# Nothing is updated until it is over. Only root's processes count, so no other
# user can hold updates off with a process that looks like it.
if pgrep -u root -f '/ubuntu-release-upgrader-[^/ ]+/' >/dev/null 2>&1; then
    log "INFO" "An Ubuntu release upgrade is running, not updating until it is over"
    exit 0
fi

# A package on hold (apt-mark hold) is the owner's choice to stay on its
# version: nothing is installed, and its APT source is left as it is.
if [ "$(dpkg-query --show --showformat='${db:Status-Want}' "${PACKAGE_NAME}" 2>/dev/null || true)" = "hold" ]; then
    log "INFO" "Package is on hold, leaving it on its version"
    exit 0
fi

# Ensure every apt operation waits for the dpkg frontend lock.
apt_get_with_lock() {
    apt-get -o DPkg::Lock::Timeout="${APT_LOCK_TIMEOUT}" "$@"
}

# Refresh only the sc-metrics-agent source so unrelated third-party APT
# repos on the host cannot block auto-updates by failing `apt-get update`.
apt_update_agent_only() {
    apt_get_with_lock update \
        -o Dir::Etc::sourcelist="${ACTIVE_SOURCE}" \
        -o Dir::Etc::sourceparts="/dev/null" \
        -o APT::Get::List-Cleanup="0"
}

# An Ubuntu release upgrade (do-release-upgrade) switches off every APT source
# that is not Ubuntu's own: a deb822 source gets "Enabled: no".
deb822_source_is_off() {
    grep -qiE '^Enabled:[[:space:]]*(no|false|off|0)[[:space:]]*$' "${DEB822_SOURCE}"
}

# Prints the first value of a field of the deb822 source. Field names are
# case-insensitive and need no space after the colon.
deb822_field() {
    awk -v field="$1" '
        { name = $0; sub(/:.*/, "", name) }
        tolower(name) == tolower(field) {
            value = $0
            sub(/^[^:]*:[[:space:]]*/, "", value)
            split(value, words, /[[:space:]]+/)
            print words[1]
            exit
        }
    ' "${DEB822_SOURCE}"
}

# Prints the source line an Ubuntu release upgrade left behind: commented out
# in its ".disabled" copy, or as it was in its ".distUpgrade" backup. A
# ".disabled" file whose line is not commented out was renamed by the owner:
# prints nothing for it.
leftover_list_line() {
    if [ -f "${DISABLED_LIST_SOURCE}" ]; then
        sed -n '/^#[[:space:]]*deb[[:space:]]/{s/^#[[:space:]]*//p;q}' "${DISABLED_LIST_SOURCE}"
    elif [ -f "${BACKUP_LIST_SOURCE}" ]; then
        sed -n '/^deb[[:space:]]/{p;q}' "${BACKUP_LIST_SOURCE}"
    fi
}

# Succeeds when APT fetches and verifies the source file $1 and finds the
# package in the repository $2, edition $3; otherwise SOURCE_ERROR says why.
# Error-Mode makes a failed download an error on the APT versions that know
# it; the policy check covers the rest.
source_answers() {
    local policy
    SOURCE_ERROR=$(apt_get_with_lock update \
        -o Dir::Etc::sourcelist="$1" \
        -o Dir::Etc::sourceparts="/dev/null" \
        -o APT::Get::List-Cleanup="0" \
        -o APT::Update::Error-Mode="any" 2>&1) || return 1
    # Matched only after apt-cache has finished: under pipefail, a grep that
    # stops at its first match would fail the pipeline.
    policy=$(apt-cache -o Dir::Etc::sourcelist="$1" -o Dir::Etc::sourceparts="/dev/null" \
        policy "${PACKAGE_NAME}" 2>/dev/null) || true
    if [[ "${policy}" != *" $2 $3"* ]]; then
        SOURCE_ERROR="${PACKAGE_NAME} is not in $2 $3"
        return 1
    fi
}

# Puts back, in deb822, the source an Ubuntu release upgrade switched off. It
# is only written once APT has verified it, so a source that cannot work never
# breaks the owner's own `apt-get update`.
restore_source() {
    local line uri suite components="" signed_by="" options="" release candidate tried=""
    local pattern='^deb[[:space:]]+(\[([^]]*)\][[:space:]]+)?([^[:space:]]+)[[:space:]]+([^[:space:]]+)([[:space:]]+(.*))?$'

    if [ -f "${DEB822_SOURCE}" ]; then
        uri=$(deb822_field URIs)
        suite=$(deb822_field Suites)
        signed_by=$(deb822_field Signed-By)
    else
        line=$(leftover_list_line)
        if [[ ! "${line}" =~ ${pattern} ]]; then
            log "ERROR" "Could not read the APT source the Ubuntu release upgrade left behind"
            return 1
        fi
        options="${BASH_REMATCH[2]}"
        uri="${BASH_REMATCH[3]}"
        suite="${BASH_REMATCH[4]}"
        components="${BASH_REMATCH[6]}"
        if [[ " ${options} " =~ [[:space:]]signed-by=([^[:space:]]+)[[:space:]] ]]; then
            signed_by="${BASH_REMATCH[1]}"
        fi
    fi
    uri="${uri%/}"
    if [ -z "${uri}" ] || [ -z "${suite}" ]; then
        log "ERROR" "Could not read the switched-off APT source"
        return 1
    fi

    if [ -n "${signed_by}" ] && [ ! -e "${signed_by}" ] && [ -f "${PACKAGED_KEYRING}" ]; then
        install -m 644 "${PACKAGED_KEYRING}" "${signed_by}"
    fi

    # Ubuntu's next release upgrade carries a source over only when it is named
    # after the release the VM runs, so that edition is tried first. A suite
    # ending in "/" is an exact path with no editions.
    local -a candidates=("${suite}")
    if [[ "${suite}" != */ ]]; then
        # shellcheck source=/dev/null
        release=$(. /etc/os-release && echo "${VERSION_CODENAME:-}")
        candidates=("${release}" "${suite}" "${FALLBACK_SUITE}")
    fi

    if ! SOURCE_CANDIDATE=$(mktemp --suffix=.sources "/run/${PACKAGE_NAME}-source.XXXXXX"); then
        log "ERROR" "Could not create a file to try the APT source in"
        return 1
    fi
    for candidate in "${candidates[@]}"; do
        if [ -z "${candidate}" ] || [[ " ${tried} " == *" ${candidate} "* ]]; then
            continue
        fi
        tried="${tried} ${candidate}"
        if [ -f "${DEB822_SOURCE}" ]; then
            sed -e '/^Enabled:/Id' -e "s|^Suites:.*|Suites: ${candidate}|I" "${DEB822_SOURCE}" > "${SOURCE_CANDIDATE}"
        else
            {
                echo "Types: deb"
                echo "URIs: ${uri}"
                echo "Suites: ${candidate}"
                if [ -n "${components}" ]; then echo "Components: ${components}"; fi
                if [ -n "${signed_by}" ]; then echo "Signed-By: ${signed_by}"; fi
            } > "${SOURCE_CANDIDATE}"
        fi
        if source_answers "${SOURCE_CANDIDATE}" "${uri}" "${candidate}"; then
            # Written under a name APT ignores, then renamed: a write that
            # fails leaves no half-written source, and keeps what the upgrade
            # left for the next run to restore from.
            if ! install -m 644 "${SOURCE_CANDIDATE}" "${DEB822_SOURCE}.dpkg-new" \
                || ! mv -f "${DEB822_SOURCE}.dpkg-new" "${DEB822_SOURCE}"; then
                rm -f "${DEB822_SOURCE}.dpkg-new"
                log "ERROR" "Could not write the APT source ${DEB822_SOURCE}"
                return 1
            fi
            # Left in place, either would bring back a source the owner
            # removes later.
            rm -f "${DISABLED_LIST_SOURCE}" "${BACKUP_LIST_SOURCE}"
            log "INFO" "Restored the APT source an Ubuntu release upgrade switched off: ${uri} ${candidate}"
            return 0
        fi
    done

    log "ERROR" "Could not restore the APT source: ${uri} did not answer for${tried}"
    log "ERROR" "${SOURCE_ERROR}"
    return 1
}

# Find the source to update from. One that an Ubuntu release upgrade switched
# off is put back. One the owner removed or renamed stays that way: there is
# nothing to update from, and nothing to fail about.
if [ -f "${DEB822_SOURCE}" ] && ! deb822_source_is_off; then
    ACTIVE_SOURCE="${DEB822_SOURCE}"
elif [ -f "${LIST_SOURCE}" ]; then
    ACTIVE_SOURCE="${LIST_SOURCE}"
elif [ -f "${DEB822_SOURCE}" ] || [ -n "$(leftover_list_line)" ]; then
    restore_source || exit 1
    ACTIVE_SOURCE="${DEB822_SOURCE}"
else
    log "INFO" "The agent's APT source was removed, not updating"
    exit 0
fi

# Add jitter to prevent thundering herd
JITTER=$((RANDOM % 60))
log "INFO" "Adding ${JITTER}s jitter delay"
sleep ${JITTER}

# Set APT environment
export DEBIAN_FRONTEND=noninteractive
export SC_AGENT_AUTO_UPDATER=1

# Function to check package health
check_package_health() {
    local status
    # An empty status is handled below as "not found", so a failing pipeline
    # must not stop the script here under set -e.
    status=$(dpkg -l ${PACKAGE_NAME} 2>/dev/null | grep "^[ih]" | awk '{print $1}') || true
    
    if [[ -z "$status" ]]; then
        log "ERROR" "Package not found in dpkg database"
        return 1
    # "hi" is a package the customer put on hold: installed and healthy. Treating
    # it as broken would reinstall it, which stops the agent and drops the hold.
    elif [[ "$status" == "ii" || "$status" == "hi" ]]; then
        log "INFO" "Package health: OK (status: ${status})"
        return 0
    elif [[ "$status" =~ [FRU] ]]; then
        log "WARN" "Package needs repair (status: ${status})"
        return 2
    else
        log "WARN" "Package status unknown: ${status}"
        return 2
    fi
}

# Function to repair package
repair_package() {
    log "INFO" "Attempting package repair..."
    
    # Try reconfiguration first
    if dpkg --configure ${PACKAGE_NAME} 2>&1; then
        log "INFO" "Package reconfiguration successful"
        return 0
    fi
    
    # Try reinstall
    log "INFO" "Reconfiguration failed, attempting reinstall..."
    if apt_get_with_lock install --reinstall -y \
        -o Dpkg::Options::="--force-confdef" \
        -o Dpkg::Options::="--force-confold" \
        ${PACKAGE_NAME} 2>&1; then
        log "INFO" "Package reinstall successful"
        return 0
    fi
    
    log "ERROR" "Package repair failed"
    return 1
}

# Check and repair package if needed. The || keeps set -e from exiting on a
# non-zero status before the repair below can run.
HEALTH_STATUS=0
check_package_health || HEALTH_STATUS=$?

if [ ${HEALTH_STATUS} -eq 1 ]; then
    log "ERROR" "Package not installed, cannot update"
    exit 1
elif [ ${HEALTH_STATUS} -eq 2 ]; then
    repair_package || exit 1
fi

# Remember if service was running before update
SERVICE_WAS_RUNNING=false
if systemctl is-active --quiet "${PACKAGE_NAME}"; then
    SERVICE_WAS_RUNNING=true
    log "INFO" "Service is currently running, will restart after update"
else
    log "INFO" "Service is not running, will remain stopped after update"
fi

# Update package lists with retry (scoped to the metrics source only)
for attempt in $(seq 1 ${MAX_RETRIES}); do
    log "INFO" "Updating package lists (attempt ${attempt}/${MAX_RETRIES})"

    # Empty APT's package cache. Another program that is downloading packages
    # (unattended-upgrades, for one) holds its lock, sometimes for minutes; the
    # check then goes on without cleaning.
    apt_get_with_lock clean 2>&1 || log "WARN" "APT's package cache is in use, not cleaning it"

    if apt_update_agent_only 2>&1; then
        log "INFO" "Package list update successful"
        break
    else
        if [ "${attempt}" -lt "${MAX_RETRIES}" ]; then
            log "WARN" "Package list update failed, retrying in ${RETRY_DELAY}s..."
            sleep ${RETRY_DELAY}
        else
            log "ERROR" "Package list update failed after ${MAX_RETRIES} attempts"
            exit 1
        fi
    fi
done

# Get version information
CURRENT_VERSION=$(dpkg-query -W -f='${Version}' ${PACKAGE_NAME} 2>/dev/null || echo "unknown")
log "INFO" "Current version: ${CURRENT_VERSION}"

# Check available version using apt-cache policy
AVAILABLE_VERSION=$(apt-cache policy ${PACKAGE_NAME} 2>/dev/null | grep "Candidate:" | awk '{print $2}' || echo "unknown")
log "INFO" "Available version: ${AVAILABLE_VERSION}"

# Verify we got valid version info
if [ "${AVAILABLE_VERSION}" = "unknown" ] || [ "${AVAILABLE_VERSION}" = "(none)" ]; then
    log "ERROR" "Could not determine available version"
    exit 1
fi

# Check if update is needed
if [ "${CURRENT_VERSION}" = "${AVAILABLE_VERSION}" ]; then
    log "INFO" "Already running latest version (${CURRENT_VERSION})"
    exit 0
fi

# Perform update
log "INFO" "Updating from ${CURRENT_VERSION} to ${AVAILABLE_VERSION}"

# Update with retry logic
UPDATE_SUCCESS=false
for attempt in $(seq 1 ${MAX_RETRIES}); do
    log "INFO" "Installation attempt ${attempt}/${MAX_RETRIES}"
    
    if apt_get_with_lock install -y --only-upgrade \
        -o Dpkg::Options::="--force-confdef" \
        -o Dpkg::Options::="--force-confold" \
        -o APT::Get::AutomaticRemove=false \
        ${PACKAGE_NAME} 2>&1; then
        
        # Verify update
        NEW_VERSION=$(dpkg-query -W -f='${Version}' ${PACKAGE_NAME} 2>/dev/null || echo "unknown")
        
        if [ "${NEW_VERSION}" != "${CURRENT_VERSION}" ] && [ "${NEW_VERSION}" != "unknown" ]; then
            log "INFO" "Update successful: ${CURRENT_VERSION} -> ${NEW_VERSION}"

            # Download latest agent configuration
            log "INFO" "Downloading latest agent configuration..."
            if [ -x "${CONFIG_DOWNLOAD_SCRIPT}" ]; then
                if "${CONFIG_DOWNLOAD_SCRIPT}"; then
                    log "INFO" "Agent configuration updated successfully"
                else
                    log "WARN" "Failed to download agent configuration, continuing with existing config"
                fi
            else
                log "WARN" "Config download script not found, skipping config update"
            fi

            UPDATE_SUCCESS=true
            break
        else
            log "ERROR" "Update verification failed (version unchanged)"
        fi
    else
        log "ERROR" "Installation attempt ${attempt} failed"
    fi
    
    if [ "${attempt}" -lt "${MAX_RETRIES}" ]; then
        log "INFO" "Retrying in ${RETRY_DELAY}s..."
        sleep ${RETRY_DELAY}
    fi
done

if [ "${UPDATE_SUCCESS}" = true ]; then
    # Configuration validation (preserving original behavior)
    if [ -f "${CONFIG_FILE}" ]; then
        log "INFO" "Validating configuration..."
        if command -v "/usr/bin/${PACKAGE_NAME}" >/dev/null 2>&1; then
            if ! "/usr/bin/${PACKAGE_NAME}" --validate-config "${CONFIG_FILE}" 2>/dev/null; then
                log "WARN" "Configuration validation failed, but continuing with update"
            else
                log "INFO" "Configuration validation successful"
            fi
        else
            log "INFO" "Binary not found, skipping config validation"
        fi
    fi

    # Check if service needs restart (using preserved logic)
    if [ "$SERVICE_WAS_RUNNING" = true ]; then
        if systemctl is-active --quiet ${PACKAGE_NAME} 2>/dev/null; then
            log "INFO" "Restarting service to apply update..."
            
            # Give service time to shut down gracefully
            if systemctl restart ${PACKAGE_NAME} 2>&1; then
                sleep 2
                
                # Verify service is running
                if systemctl is-active --quiet ${PACKAGE_NAME} 2>/dev/null; then
                    log "INFO" "Service restarted successfully"
                else
                    log "ERROR" "Service failed to restart after update"
                    log "INFO" "Attempting to start service..."
                    systemctl start ${PACKAGE_NAME} 2>&1 || true
                fi
            else
                log "ERROR" "Failed to restart service"
            fi
        else
            log "INFO" "Service was running but not active after update, attempting to start..."
            systemctl start ${PACKAGE_NAME} 2>&1 || true
        fi
    else
        log "INFO" "Service was not running before update, leaving stopped"
    fi
    
    log "INFO" "Update completed successfully"
    exit 0
else
    log "ERROR" "Update failed after ${MAX_RETRIES} attempts"
    exit 1
fi