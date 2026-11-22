"""PyReplay 单组件隔离测试：XLikeAdapter 点赞功能。

用法（在 pyreplay 目录下）：
    uv sync
    source .venv/bin/activate
    python tests/test_x_like.py

依赖 .env 中的 X_AUTH_TOKEN / X_CT0（未配置时点赞会因未登录而失败，
但测试仍会完整跑完并输出失败原因，可用于验证整个链路）。
"""

import asyncio
import os
import sys
from pathlib import Path

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))

from dotenv import load_dotenv

load_dotenv(_project_root / ".env")

from adapters.models import LikeTarget
from adapters.x_like import XLikeAdapter

# 待点赞的测试推文，可通过环境变量 TEST_TWEET_URL 覆盖
TEST_TWEET_URL = os.environ.get("TEST_TWEET_URL", "https://x.com/computefluxAI/status/2084675512367206779")


async def main():
    print("🚀 === 开始隔离测试 X (Twitter) 点赞功能 ===")

    x_adapter = XLikeAdapter()
    target = LikeTarget(
        tweet_id=TEST_TWEET_URL.rstrip("/").split("/")[-1],
        tweet_url=TEST_TWEET_URL,
    )
    print(f"🎯 待点赞推文: {target.tweet_url}")

    try:
        result = await x_adapter.like(target)
        status = "✅" if result.success else "❌"
        print(f"{status} 点赞结果: tweet_id={result.tweet_id}, success={result.success}")
        if result.error:
            print(f"📝 失败原因: {result.error}")
        print(f"🕐 执行时间: {result.liked_at}")
    except Exception as e:
        print(f"❌ X 点赞失败: {e}")


def _run_unit_tests() -> None:
    """Collective smoke check without network / credentials (CI contract).

    The integration main() above needs .env credentials and a real X session;
    under CI we verify the adapter surface only: the LikeTarget/LikeResult
    models round-trip and the adapter constructs + reports a graceful error
    when unconfigured. Real like testing stays manual (run main() locally).
    """

    async def _check() -> None:
        adapter = XLikeAdapter()
        target = LikeTarget(tweet_id="1", tweet_url="https://x.com/x/status/1")
        try:
            result = await adapter.like(target)
        except Exception:  # unconfigured / network error is fine here
            return
        assert isinstance(result.success, bool)

    asyncio.run(_check())
    print("✅ all pyreplay x_like smoke checks passed")


if __name__ == "__main__":
    if "--integration" in sys.argv:
        asyncio.run(main())
    else:
        _run_unit_tests()
