FROM nginx:1.30.8-alpine@sha256:8eb04fe3b385090e9aa02961f77398ad09b6bf33ab07f6669a5bb7acce22c0d3 AS runtime-base
RUN apk upgrade --no-cache

FROM runtime-base AS runtime
COPY deploy/production/integrated/nginx.conf.template /etc/nginx/ocservia.conf.template
COPY deploy/production/integrated/edge-entrypoint.sh /usr/local/bin/ocservia-edge
RUN nginx -V 2>&1 | grep -q -- --with-stream_ssl_preread_module \
    && chmod 0555 /usr/local/bin/ocservia-edge
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ocservia-edge"]
EXPOSE 8443
