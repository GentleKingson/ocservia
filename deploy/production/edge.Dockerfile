FROM nginx:1.28.3-alpine@sha256:a8b39bd9cf0f83869a2162827a0caf6137ddf759d50a171451b335cecc87d236
COPY deploy/production/integrated/nginx.conf.template /etc/nginx/ocservia.conf.template
COPY deploy/production/integrated/edge-entrypoint.sh /usr/local/bin/ocservia-edge
RUN nginx -V 2>&1 | grep -q -- --with-stream_ssl_preread_module \
    && chmod 0555 /usr/local/bin/ocservia-edge
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ocservia-edge"]
EXPOSE 8443
