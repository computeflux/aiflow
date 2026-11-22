import asyncio
import os
import sys
from pathlib import Path

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))

from adapters.x import XAdapter


async def main():
    print("🚀 === 开始隔离测试 X (Twitter) 爬取功能 ===")
    os.environ["X_USE_SCRAPER"] = "true"

    x_adapter = XAdapter(headless=True)
    try:
        x_items = await x_adapter.fetch(keyword="AI", limit=30, mode="keyword")
        print(f"✅ X 抓取成功，共获得 {len(x_items)} 条原始数据：")
        for item in x_items:
            print(f"--- \n🔗 Tweet URL: {item.url}\n📝 內容: {item.content}\n")
    except Exception as e:
        print(f"❌ X 抓取失败: {e}")


if __name__ == "__main__":
    asyncio.run(main())
