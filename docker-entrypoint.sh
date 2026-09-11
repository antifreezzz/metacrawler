#!/bin/sh
set -e

# Директория для БД
DB_DIR=$(dirname "${DB_PATH:-/app/data/metacrawler.db}")

mkdir -p "$DB_DIR"
chown -R appuser:appgroup "$DB_DIR" 2>/dev/null || true
chmod 700 "$DB_DIR" 2>/dev/null || true

# Если файл БД уже существует, выставляем права и на него
if [ -f "${DB_PATH:-/app/data/metacrawler.db}" ]; then
    chown appuser:appgroup "${DB_PATH:-/app/data/metacrawler.db}"* 2>/dev/null || true
    chmod 600 "${DB_PATH:-/app/data/metacrawler.db}"* 2>/dev/null || true
fi

# Запуск приложения от непривилегированного пользователя appuser
if [ "$#" -gt 0 ]; then
    exec su-exec appuser:appgroup "$@"
else
    exec su-exec appuser:appgroup /app/server
fi
