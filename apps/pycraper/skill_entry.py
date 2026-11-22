#!/usr/bin/env python3
"""'x-scan' skill 入口（P1 Skill 框架 pilot，docs/plan/transform-plan.md §6.6）。

两种调用方式：
  1. service 型（当前部署）：pycraper 内置 HTTP 端点 POST :9504/invoke 复用 run_scan()
     （S0 部署统一后 workflow-api 容器内无 docker CLI，原 docker exec -i 方式不可用）；
  2. cli 型（保留兼容）：python3 skill_entry.py，输入 JSON 走 stdin，结果 JSON 走 stdout。

输入：
    {"handle": "computefluxAI", "limit": 10}
输出：
    {"ok": true, "handle": "computefluxAI", "count": 3, "items": [<RawItem JSON>...]}
"""
from __future__ import annotations

import asyncio
import json
import sys
import traceback
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from adapters.x import XAdapter  # noqa: E402

_MAX_LIMIT = 50


async def run_scan(body: Any) -> dict[str, Any]:
    """x-scan 核心逻辑：按 handle 抓取 X 用户最新推文，返回结构化结果 dict。"""
    if not isinstance(body, dict):
        raise ValueError("input must be a JSON object")

    handle = str(body.get("handle") or "").lstrip("@").strip()
    if not handle:
        raise ValueError("handle is required")

    limit = int(body.get("limit") or 10)
    limit = max(1, min(limit, _MAX_LIMIT))

    adapter = XAdapter()
    items = await adapter.fetch(handle, limit, mode="user")
    # fetch 内部把 limit 当作滚动目标而非输出上限，这里严格截断到请求数量
    if len(items) > limit:
        items = items[:limit]

    return {
        "ok": True,
        "handle": handle,
        "count": len(items),
        "items": [item.model_dump(mode="json") for item in items],
    }


def main() -> None:
    try:
        raw = sys.stdin.read()
        out = asyncio.run(run_scan(json.loads(raw or "{}")))
        print(json.dumps(out, ensure_ascii=False, default=str), flush=True)
    except Exception as exc:
        # 任何异常都输出结构化失败并退出 1，由 Gateway 记录 skill_runs(status=fail)
        sys.stderr.write(f"{exc}\n{traceback.format_exc()}\n")
        print(json.dumps({"ok": False, "reason": str(exc)}, ensure_ascii=False), flush=True)
        sys.exit(1)


if __name__ == "__main__":
    main()
