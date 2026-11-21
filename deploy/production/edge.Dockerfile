FROM nginx:1.30.7-alpine@sha256:caf4e1e032a20f9c3c5e28fc7f25ffd768ad8677b4975c54193075997b3a97e3 AS runtime
RUN apk upgrade --no-cache brotli-libs curl libcurl libexpat libx11 pcre2 tiff zlib \
    && apk add --no-cache --upgrade 'libcrypto3>=3.5.9-r1' 'libssl3>=3.5.9-r1'
COPY deploy/production/integrated/nginx.conf.template /etc/nginx/ocservia.conf.template
COPY deploy/production/integrated/edge-entrypoint.sh /usr/local/bin/ocservia-edge
RUN nginx -V 2>&1 | grep -q -- --with-stream_ssl_preread_module \
    && chmod 0555 /usr/local/bin/ocservia-edge
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ocservia-edge"]
EXPOSE 8443
