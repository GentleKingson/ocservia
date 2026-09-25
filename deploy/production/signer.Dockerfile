FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS build
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
