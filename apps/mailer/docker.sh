#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

IMAGE_NAME="workflow-mailer:latest"
CONTAINER_NAME="workflow-mailer"
PORT="${MAILER_PORT:-8003}"

echo "Building Docker image: $IMAGE_NAME..."
docker build -t "$IMAGE_NAME" .

if [ "$(docker ps -aq -f name=^/${CONTAINER_NAME}$)" ]; then
    echo "Stopping and removing existing container: $CONTAINER_NAME..."
    docker rm -f "$CONTAINER_NAME" || true
fi

echo "Starting container: $CONTAINER_NAME on port $PORT..."
# 必须加入统一 compose 网络（workflow-net），否则 temporal:7233 / workflow-mysql 等容器名无法解析
NETWORK="${MAILER_NETWORK:-workflow-net}"
docker network inspect "$NETWORK" >/dev/null 2>&1 || docker network create "$NETWORK"
docker run -d \
  --name "$CONTAINER_NAME" \
  --restart unless-stopped \
  -p "127.0.0.1:${PORT}:${PORT}" \
  --network "$NETWORK" \
  --env-file ../../.env \
  "$IMAGE_NAME"

echo "Mailer container started successfully!"
