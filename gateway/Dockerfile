# piso gateway — MITM egress proxy + control plane + ingress reverse proxy.
# Built as a static binary in a scratch-ish alpine image: minimal attack surface
# (it is the trust root: holds real secrets, the MITM CA key, and the log).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY gateway/ ./gateway/
RUN cd gateway && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/gateway /usr/local/bin/gateway
# secrets/CA/log live under /data (host-mounted, gateway-only)
VOLUME ["/data"]
WORKDIR /data
EXPOSE 8080 8081 8082
ENTRYPOINT ["/usr/local/bin/gateway"]