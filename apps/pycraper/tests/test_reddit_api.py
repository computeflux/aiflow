"""Test the official Reddit API adapter (requires credentials)."""

import asyncio
import sys
from pathlib import Path

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))

from adapters.reddit_api import RedditAPIAdapter


async def main():
    print("🚀 === Reddit API 抓取测试（官方 API）===")

    adapter = RedditAPIAdapter()

    # Test 1: fetch a subreddit
    print("\n─── Test 1: subreddit mode ───")
    try:
        items = await adapter.fetch(keyword="cryptocurrency", limit=5, mode="subreddit")
        print(f"✅ 抓取 {len(items)} 条")
        for item in items[:3]:
            print(f"   [{item.keyword}] {item.content[:80]}...")
    except Exception as e:
        print(f"❌ 失败: {e}")

    # Test 2: search by keyword
    print("\n─── Test 2: search mode ───")
    try:
        items = await adapter.fetch(keyword="TEE privacy", limit=5, mode="search")
        print(f"✅ 搜索到 {len(items)} 条")
        for item in items[:3]:
            print(f"   [{item.keyword}] {item.content[:80]}...")
    except Exception as e:
        print(f"❌ 失败: {e}")

    # Test 3: discover subreddits
    print("\n─── Test 3: discover mode ───")
    try:
        items = await adapter.fetch(keyword="machine learning", limit=8, mode="discover")
        print(f"✅ 发现并抓取 {len(items)} 条")
        for item in items[:3]:
            print(f"   [{item.keyword}] {item.content[:80]}...")
    except Exception as e:
        print(f"❌ 失败: {e}")

    print("\n🏁 测试完成")


if __name__ == "__main__":
    asyncio.run(main())
