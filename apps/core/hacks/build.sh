#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# build.sh  —  编译 core 模块 Go 二进制文件到 bin/
# （2026-10-10 仓库重组后本脚本只覆盖 apps/core 主模块；
#   c2c / pycraper 等独立子项目各有自己的构建流程，见其 AGENTS.md）
# ============================================================

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

echo "==> 编译 Go 二进制文件..."

cd "$PROJECT_DIR"

mkdir -p bin

# API server
echo "  -> bin/api"
go build -o bin/api ./cmd/api/

# Go Temporal worker
echo "  -> bin/go-worker"
go build -o bin/go-worker ./cmd/workflow/

echo "==> 编译完成"
ls -lh bin/
