FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ocserv sqlite3 systemd systemd-sysv dbus sudo openssl ca-certificates python3 procps \
    && rm -rf /var/lib/apt/lists/*
CMD ["/sbin/init"]
