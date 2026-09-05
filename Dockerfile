# Multi-stage Dockerfile for Metacrawler (Pure Go SQLite, Alpine)

# 1. Builder Stage
FROM golang:alpine AS builder

WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY web/ web/

# Сборка статического бинарника без CGO
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/server ./cmd/server

# 2. Production Runner Stage
FROM alpine:3.21

WORKDIR /app

# Устанавливаем su-exec для корректного сброса прав после монтирования томов
RUN apk add --no-cache ca-certificates tzdata su-exec && \
    addgroup -S appgroup && adduser -S appuser -G appgroup && \
    mkdir -p /app/data && chown -R appuser:appgroup /app

# Копирование собранного бинарника, шаблонов и entrypoint скрипта
COPY --from=builder /bin/server /app/server
COPY --from=builder /src/web/templates /app/web/templates
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV PORT=8080
ENV DB_PATH=/app/data/metacrawler.db

EXPOSE 8080
VOLUME ["/app/data"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/server"]
