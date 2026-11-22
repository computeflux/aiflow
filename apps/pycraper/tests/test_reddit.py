import asyncio
import sys
from pathlib import Path

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))

from adapters.reddit import RedditAdapter

TEST_SUBS = ["cryptocurrency"]


async def main():
    print("🚀 === Reddit 频道抓取测试 ===")

    adapter = RedditAdapter(headless=False, debug=True)
    total = 0

    for sub in TEST_SUBS:
        print(f"\n─── r/{sub} ───")
        try:
            items = await adapter.fetch(keyword=sub, limit=100, mode="subreddit")
            print(f"✅ 抓取 {len(items)} 条")
            for item in items[:3]:
                print(f"   [{item.keyword}] {item.content[:80]}")
            total += len(items)
        except Exception as e:
            print(f"❌ 失败: {e}")

    print(f"\n🏁 总计 {total} 条")


if __name__ == "__main__":
    asyncio.run(main())
