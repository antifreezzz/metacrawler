# Multi-stage Dockerfile for Metacrawler (Pure Go SQLite, Alpine)

# 1. Builder Stage
FROM golang:1.24-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git ca-certificates

# Кэширование зависимостей
COPY go.mod go.sum ./
RUN go mod download

# Копирование исходников
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/ web/

# Сборка статического бинарника (pure Go SQLite modernc.org/sqlite, CGO=0)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/server ./cmd/server

# 2. Production Runner Stage
FROM alpine:3.21

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S appgroup && adduser -S appuser -G appgroup && \
    mkdir -p /app/data && chown -R appuser:appgroup /app

# Копирование собранного бинарника и шаблонов
COPY --from=builder /bin/server /app/server
COPY --from=builder /src/web/templates /app/web/templates

USER appuser

ENV PORT=8080
ENV DB_PATH=/app/data/metacrawler.db

EXPOSE 8080
VOLUME ["/app/data"]

CMD ["/app/server"]
