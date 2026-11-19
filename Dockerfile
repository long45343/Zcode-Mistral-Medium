# Builds a minimal runtime image around a pre-built Linux binary.
#
# The binary is compiled on the build machine (GOOS=linux GOARCH=amd64) because
# the target host has no buildx and pulling a Go toolchain image there is slow.
#
#   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o gateway .
#   docker build -t zcode-cache-gateway:latest .
#
# ca-certificates.crt is copied in rather than installed, because `apk add`
# needs network access that the build host does not provide. Produce it with:
#   cp /etc/ssl/certs/ca-certificates.crt .

FROM alpine:3.20

# TLS roots for the HTTPS call to the provider.
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

WORKDIR /app

COPY gateway /app/gateway
RUN chmod +x /app/gateway

# Settings live here so the dashboard can persist them across restarts.
VOLUME ["/data"]

EXPOSE 8787

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8787/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/data/settings.json"]
