# Multi-stage Dockerfile for Metacrawler (Pure Go SQLite, Alpine)

# 1. Builder Stage
FROM golang:1.27.1-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS builder

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
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d

ARG VERSION=dev
LABEL org.opencontainers.image.revision="${VERSION}"

WORKDIR /app

# yt-dlp пинится по версии и SHA256: образ воспроизводим и не доверяет
# изменяемому артефакту latest.
ARG YTDLP_VERSION=2026.08.19
ARG YTDLP_SHA256=1fa6733c37ea6fb51c99ad8fe785e7b7e5f3246c9b980230329d4fb72ed8d4d6

# Устанавливаем su-exec, nodejs, ffmpeg и зафиксированный бинарник yt-dlp для
# решения JS-челленджей YouTube и обработки аудио.
RUN apk add --no-cache ca-certificates tzdata su-exec nodejs ffmpeg curl python3 && \
    curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp" -o /usr/local/bin/yt-dlp && \
    echo "${YTDLP_SHA256}  /usr/local/bin/yt-dlp" | sha256sum -c - && \
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
