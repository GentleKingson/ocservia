FROM golang:1.26.5-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599 AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY control-plane/go.mod control-plane/go.sum ./control-plane/
WORKDIR /src/control-plane
RUN go mod download
COPY control-plane/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/ocserv-control ./cmd/ocserv-control

FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS runtime-base
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 ocservia \
    && usermod --gid ocservia nobody \
    && install -d -o nobody -g ocservia -m 0750 /run/ocserv-trust
USER nobody:ocservia

FROM runtime-base
COPY --from=build /out/ocserv-control /usr/local/bin/ocserv-control
ENTRYPOINT ["/usr/local/bin/ocserv-control"]
CMD ["--role=all"]
