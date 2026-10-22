# Compatibility fixture only; production/release images keep their 1.5.0 baseline.
# Debian 13 multi-architecture index, resolved 2026-10-22.
FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
COPY scripts/build-ocserv160-compat.sh /tmp/build-ocserv160-compat.sh
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export CURL_CA_BUNDLE=/run/secrets/proxy_ca; fi; \
    OCSERVIA_COMPAT_DISPOSABLE=1 bash /tmp/build-ocserv160-compat.sh \
    && apt-get install -y --no-install-recommends openconnect openssl ssl-cert iproute2 binutils python3 \
    && rm -rf /var/lib/apt/lists/* /tmp/build-ocserv160-compat.sh
