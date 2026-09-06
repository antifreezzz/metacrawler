#!/usr/bin/env bash
set -e

# Copy this file to deploy.sh (ignored by git) and set your server details:
# cp deploy.example.sh deploy.sh
VPS_HOST="${VPS_HOST:-user@your-vps-ip}"
VPS_PATH="${VPS_PATH:-/opt/metacrawler}"

# Make sure we're on main
CURRENT_BRANCH=$(git rev-parse --abbrev-ref HEAD)
if [ "$CURRENT_BRANCH" != "main" ]; then
    echo "⚠️ Current branch is '$CURRENT_BRANCH'. Switching to 'main'..."
    git checkout main
fi

echo "📥 Pulling latest changes from origin/main..."
git pull --rebase origin main

echo "🚀 Pushing changes to origin/main..."
git push origin main

echo "🔄 Updating on VPS..."
ssh "$VPS_HOST" "cd $VPS_PATH && git pull && docker compose up -d --build"

echo "✅ Deploy completed successfully!"
