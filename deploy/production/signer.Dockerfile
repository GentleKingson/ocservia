FROM golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS build
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
