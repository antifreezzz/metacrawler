#!/usr/bin/env bash
set -euo pipefail

# Copy this file to deploy.sh (ignored by git) and set your server details:
# cp deploy.example.sh deploy.sh
VPS_HOST="${VPS_HOST:-user@your-vps-ip}"
VPS_PATH="${VPS_PATH:-/opt/metacrawler}"
IMAGE_PREFIX="${IMAGE_PREFIX:-metacrawler}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8079/healthz}"

REVISION="$(git rev-parse --short HEAD 2>/dev/null || true)"
if [ -z "$REVISION" ]; then
    echo "Error: not a git repository, cannot derive revision" >&2
    exit 1
fi

echo "==> Pushing main"
git push origin main

echo "==> Deploying ${IMAGE_PREFIX}:${REVISION} on ${VPS_HOST}"

# Делегируем сборку и переключение на сервер. Деплой health-gated: если
# /healthz не поднялся, откатываемся на предыдущий образ.
ssh "${VPS_HOST}" \
    "VPS_PATH='${VPS_PATH}' IMAGE_PREFIX='${IMAGE_PREFIX}' REVISION='${REVISION}' HEALTH_URL='${HEALTH_URL}' bash -s" <<'REMOTE'
set -euo pipefail
cd "$VPS_PATH"

echo "--> Syncing origin/main"
git fetch origin
git checkout main
git reset --hard origin/main

PREV_IMAGE="$(docker inspect --format '{{.Config.Image}}' metacrawler 2>/dev/null || true)"
echo "$PREV_IMAGE" > .last_deploy
echo "--> Previous image: ${PREV_IMAGE:-<none>}"

echo "--> Building ${IMAGE_PREFIX}:${REVISION}"
IMAGE="${IMAGE_PREFIX}:${REVISION}" docker compose build

echo "--> Starting ${IMAGE_PREFIX}:${REVISION}"
IMAGE="${IMAGE_PREFIX}:${REVISION}" docker compose up -d

echo "--> Waiting for health at ${HEALTH_URL}"
for _ in $(seq 1 30); do
    if curl -fsS "$HEALTH_URL" >/dev/null 2>&1; then
        echo "--> Health check passed"
        exit 0
    fi
    sleep 2
done

echo "--> Health check FAILED, rolling back" >&2
if [ -n "$PREV_IMAGE" ]; then
    IMAGE="$PREV_IMAGE" docker compose up -d
    echo "--> Rolled back to $PREV_IMAGE" >&2
fi
exit 1
REMOTE

echo "==> Deploy complete"
