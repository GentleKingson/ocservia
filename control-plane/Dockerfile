FROM golang:1.26.5-bookworm AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY control-plane/go.mod control-plane/go.sum ./control-plane/
WORKDIR /src/control-plane
RUN go mod download
COPY control-plane/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/ocserv-control ./cmd/ocserv-control

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 ocservia \
    && usermod --gid ocservia nobody
COPY --from=build /out/ocserv-control /usr/local/bin/ocserv-control
USER nobody:ocservia
ENTRYPOINT ["/usr/local/bin/ocserv-control"]
CMD ["--role=all"]
