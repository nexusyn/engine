# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download

COPY cmd/        cmd/
COPY internal/   internal/
COPY migrations/ migrations/

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/nexus ./cmd/nexus

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S nexus && \
    adduser -S -G nexus -u 10001 nexus

WORKDIR /app
COPY --from=builder /out/nexus /usr/local/bin/nexus
COPY --from=builder /src/migrations /app/migrations

USER nexus
EXPOSE 8044

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/nexus", "health-check"]

ENTRYPOINT ["/usr/local/bin/nexus"]
CMD ["serve"]
