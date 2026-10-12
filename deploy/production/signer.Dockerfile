FROM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
WORKDIR /src/signer
COPY signer/go.mod signer/go.sum ./
RUN go mod download
COPY signer/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ocserv-signer .

FROM scratch
COPY --from=build /out/ocserv-signer /ocserv-signer
USER 65532:65532
ENTRYPOINT ["/ocserv-signer"]
CMD ["serve"]
EXPOSE 9443
