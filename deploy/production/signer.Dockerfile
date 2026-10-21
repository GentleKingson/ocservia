FROM golang:1.26.10-bookworm@sha256:fe3da92a5cb2881e4aa75e625cb0b7c50571632fb4cf340cbe53e6ccdcffdd51 AS build
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
