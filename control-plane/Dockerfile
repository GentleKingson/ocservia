FROM golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS build
WORKDIR /src
COPY control-plane/go.mod control-plane/go.sum ./control-plane/
WORKDIR /src/control-plane
RUN go mod download
COPY control-plane/ .
ARG VERSION=dev
ARG COMMIT=unknown
RUN --mount=type=cache,id=ocservia-control-gobuild,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/ocserv-control ./cmd/ocserv-control
RUN --mount=type=cache,id=ocservia-control-gobuild,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ocserv-sealing-export ./cmd/ocserv-sealing-export

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS runtime-base
RUN apt-get -o APT::Update::Error-Mode=any update \
    && apt-get -o Acquire::Retries=0 install -y --no-install-recommends --only-upgrade apt libapt-pkg6.0 \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' apt)" ge 2.6.2 \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && apt-get install -y --no-install-recommends --only-upgrade perl-base libpcre2-8-0 \
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
