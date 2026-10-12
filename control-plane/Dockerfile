FROM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY control-plane/go.mod control-plane/go.sum ./control-plane/
WORKDIR /src/control-plane
RUN go mod download
COPY control-plane/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/ocserv-control ./cmd/ocserv-control
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ocserv-sealing-export ./cmd/ocserv-sealing-export

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS runtime-base
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 ocservia \
    && usermod --gid ocservia nobody \
    && install -d -o nobody -g ocservia -m 0750 /run/ocserv-trust
USER nobody:ocservia

FROM runtime-base
COPY --from=build /out/ocserv-control /usr/local/bin/ocserv-control
COPY --from=build /out/ocserv-sealing-export /usr/local/bin/ocserv-sealing-export
COPY control-plane/internal/auth/password_blocklist/LICENSE.django /usr/share/doc/ocservia/LICENSE.django
COPY THIRD_PARTY_NOTICES.md /usr/share/doc/ocservia/THIRD_PARTY_NOTICES.md
ENTRYPOINT ["/usr/local/bin/ocserv-control"]
CMD ["--role=all"]
