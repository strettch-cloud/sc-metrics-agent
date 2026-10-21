# One OS with systemd as PID 1, for the package lifecycle tests.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

# The package's dependencies are installed here, so installing the .deb in a
# test needs no network.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends \
        systemd systemd-sysv ca-certificates curl jq dmidecode procps \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# A container can't load kernel modules, so this unit always fails and would
# leave systemd "degraded".
RUN systemctl mask systemd-modules-load.service

# systemd shuts down cleanly on this signal, not on docker's default SIGTERM.
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
