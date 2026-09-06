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

# Устанавливаем su-exec, nodejs, ffmpeg и актуальный бинарник yt-dlp для решения JS-челленджей YouTube и обработки аудио
RUN apk add --no-cache ca-certificates tzdata su-exec nodejs ffmpeg curl python3 && \
    curl -L https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp && \
    chmod a+rx /usr/local/bin/yt-dlp && \
    addgroup -S appgroup && adduser -S appuser -G appgroup && \
    mkdir -p /app/data && chown -R appuser:appgroup /app

# Копирование собранного бинарника, шаблонов и entrypoint скрипта
COPY --from=builder /bin/server /app/server
COPY --from=builder /src/web/templates /app/web/templates
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV PORT=8079
ENV DB_PATH=/app/data/metacrawler.db
ENV TZ=Europe/Moscow

EXPOSE 8079
VOLUME ["/app/data"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/server"]
