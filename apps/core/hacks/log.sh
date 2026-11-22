#!/usr/bin/env bash
set -euo pipefail

LINES="${2:-50}"; WHAT="${1:-help}"

case "$WHAT" in
  py|py-worker|python)
    journalctl -fu workflow-py-worker -o cat -n "$LINES" ;;
  go|go-worker)
    journalctl -fu workflow-go-worker -o cat -n "$LINES" 2>/dev/null || \
      tail -f -n "$LINES" /tmp/workflow-go.log ;;
  api)
    journalctl -fu workflow-api -o cat -n "$LINES" 2>/dev/null || \
      tail -f -n "$LINES" /tmp/workflow-api.log ;;
  board)
    tail -f -n "$LINES" /tmp/workflow-board.log 2>/dev/null || echo "no board log" ;;
  all)
    echo "=== py ==="; journalctl -u workflow-py-worker -o cat -n 20 2>/dev/null || true; echo
    echo "=== go ==="; journalctl -u workflow-go-worker -o cat -n 10 2>/dev/null || true; echo
    echo "=== api ==="; journalctl -u workflow-api -o cat -n 10 2>/dev/null || true ;;
  *)
    echo "用法: $0 {py|go|api|board|all} [lines]" ;;
esac
