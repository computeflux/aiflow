#!/bin/bash
# Poll GitHub for new commits and auto-rebuild the Python worker containers.
# Install: cp this to /usr/local/bin/computeflux-auto-update.sh
#           systemctl enable --now computeflux-auto-update.timer

set -euo pipefail

# Repo root: 2026-10-10 仓库重组后 hacks 在 apps/core/hacks，monorepo 根在上一级的上一级
REPO_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
SHA_FILE="/tmp/computeflux-workflow-last-sha"

# Fetch latest commit SHA from GitHub API (no auth needed for public repos)
LATEST_SHA=$(curl -sf https://api.github.com/repos/computeflux/workflow/commits?per_page=1 \
  | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['sha'])")

if [ -z "$LATEST_SHA" ]; then
  echo "WARNING: failed to fetch latest SHA"
  exit 0
fi

if [ -f "$SHA_FILE" ] && [ "$(cat "$SHA_FILE")" = "$LATEST_SHA" ]; then
  exit 0
fi

echo "$(date): new commit $LATEST_SHA detected, pulling..."
cd "$REPO_DIR"
git pull --ff-only origin main 2>&1 || true

echo "$LATEST_SHA" > "$SHA_FILE"
# pycraper / pyreplay 已容器化：git pull 后需重建镜像并重建容器
bash "$REPO_DIR/hacks/docker-workers.sh" rebuild
echo "$(date): done"
