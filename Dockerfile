# Build stage
FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy the manifests first so the dependency layer is cached and only
# re-downloaded when go.mod/go.sum actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a static binary, which is what lets the final image be
# scratch-thin. -ldflags "-s -w" strips the symbol table and DWARF data.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /out/api ./cmd/api

# Runtime stage
FROM alpine:3.20

# ca-certificates for outbound TLS; tzdata so timestamps are not all UTC-only.
RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 appuser

WORKDIR /app
COPY --from=build /out/api /app/api

# Run unprivileged: a container process that does not need root should not have it.
USER appuser

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=3 \
  CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/api"]
