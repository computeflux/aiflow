import asyncio
import sys
from pathlib import Path

# Ensure project root is on sys.path so ``from adapters...`` imports work
_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))

from dotenv import load_dotenv

from adapters.github import GitHubAdapter

load_dotenv(_project_root / ".env")


async def main():
    print("🚀 === 开始隔离测试 GitHub 爬取功能 ===")

    github_adapter = GitHubAdapter()
    try:
        github_items = await github_adapter.fetch(org="elizaOS", limit=1000)
        print(f"✅ GitHub 抓取成功，共获得 {len(github_items)} 条原始数据：")
        for item in github_items:
            print(f"--- \n🔗 URL: {item.url}\n📝 內容摘要:\n{item.content}\n")
    except Exception as e:
        print(f"❌ GitHub 抓取失败: {e}")


if __name__ == "__main__":
    asyncio.run(main())
