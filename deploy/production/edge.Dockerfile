FROM nginx:1.30.5-alpine@sha256:bf3201ab56f23e5954646379c775d511fc466e9f11376d9725361064ad07ed35
RUN apk upgrade --no-cache libexpat
COPY deploy/production/integrated/nginx.conf.template /etc/nginx/ocservia.conf.template
COPY deploy/production/integrated/edge-entrypoint.sh /usr/local/bin/ocservia-edge
RUN nginx -V 2>&1 | grep -q -- --with-stream_ssl_preread_module \
    && chmod 0555 /usr/local/bin/ocservia-edge
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ocservia-edge"]
EXPOSE 8443
