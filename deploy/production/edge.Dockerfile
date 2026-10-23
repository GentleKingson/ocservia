FROM nginx:1.30.6-alpine@sha256:47928bf67eb62a1863cb08d9ecb52d3e704304ba614eaee17884204677702951 AS runtime
RUN apk upgrade --no-cache libexpat libx11 pcre2 tiff zlib
COPY deploy/production/integrated/nginx.conf.template /etc/nginx/ocservia.conf.template
COPY deploy/production/integrated/edge-entrypoint.sh /usr/local/bin/ocservia-edge
RUN nginx -V 2>&1 | grep -q -- --with-stream_ssl_preread_module \
    && chmod 0555 /usr/local/bin/ocservia-edge
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ocservia-edge"]
EXPOSE 8443
