#!/bin/sh
set -eu

# 由 deploy/compose/infra/temporal.yml 挂载（compose 挂载源，勿挪位置）。
# 创建所有 namespace：default（存量）+ agent-prod（S2 混合编排，retention 7d）。
# 幂等：已存在则跳过。
TEMPORAL_ADDRESS=${TEMPORAL_ADDRESS:-temporal:7233}

echo "Waiting for Temporal server..."
sleep 5

for NAMESPACE in default agent-prod; do
  case "$NAMESPACE" in
    agent-prod) EXTRA_ARGS="--retention 7d" ;;
    *)          EXTRA_ARGS="" ;;
  esac
  echo "Creating namespace '$NAMESPACE'..."
  if temporal operator namespace describe -n "$NAMESPACE" --address "$TEMPORAL_ADDRESS" 2>/dev/null; then
    echo "Namespace '$NAMESPACE' already exists"
  else
    # shellcheck disable=SC2086
    temporal operator namespace create -n "$NAMESPACE" $EXTRA_ARGS --address "$TEMPORAL_ADDRESS"
    echo "Namespace '$NAMESPACE' created"
  fi
done
