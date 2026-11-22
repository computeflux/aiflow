"""Xiaohongshu adapter tests and manual MCP integration script.

Pytest coverage for the unconfigured (mcp_command == 'echo') behavior:
- an INFO log line is emitted at startup when the MCP server is not configured;
- fetch silently returns an empty list (design preserved);
- publish raises an explicit error (no fake success);
- the shipped config template (xiaohongshu.json.example) is valid JSON with an
  empty mcp_command placeholder.

Manual integration usage (real requests, test account only, single fetch + single publish):
    cd apps/pycraper
    uv run python tests/test_xiaohongshu.py            # 仅抓取（search_notes）
    uv run python tests/test_xiaohongshu.py --publish  # 抓取 + 发布（publish_note）

前置：config/xiaohongshu.json 的 mcp_command 已填真实 MCP server 命令
（或设置 MCP_XHS_COMMAND 环境变量覆盖），且 MCP server 已登录测试账号。
边界：仅使用测试账号，单次调用，不群发、不批量刷。
"""

from __future__ import annotations

import asyncio
import json
import logging
import sys
import types
from pathlib import Path

try:
    import pytest
except ModuleNotFoundError:  # dependency-less sandbox (CI contract fallback)
    _pytest = types.ModuleType("pytest")

    def _raises(expected, *_a, **_k):
        import contextlib

        @contextlib.contextmanager
        def _ctx():
            try:
                yield
            except expected:
                return
            raise AssertionError(f"DID NOT RAISE {expected!r}")

        return _ctx()

    _pytest.raises = _raises  # type: ignore[attr-defined]

    def _fixture(func=None, **_k):
        if func is not None:
            return func
        return lambda f: f

    _pytest.fixture = _fixture  # type: ignore[attr-defined]
    pytest = _pytest  # type: ignore[assignment]

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))


def _ensure_adapters_importable() -> None:
    """Make ``adapters`` importable in dependency-less sandboxes (CI contract).

    Same convention as tests/test_xiaohongshu_publish.py: when the mcp SDK
    (pulled in by adapters.mcp_client) is not installed, install a minimal
    stub module for the seam BEFORE importing the adapter so collection and
    the unconfigured-state unit tests work hermetically. The manual
    integration main() at the bottom requires the real package.
    """
    try:
        import adapters.mcp_client  # noqa: F401  (real package when installed)
        return
    except Exception:
        pass

    pkg_name = "adapters"
    if pkg_name not in sys.modules:
        pkg = types.ModuleType(pkg_name)
        pkg.__path__ = [str(_project_root / "adapters")]  # type: ignore[attr-defined]
        sys.modules[pkg_name] = pkg
    mc_name = pkg_name + ".mcp_client"
    mc = types.ModuleType(mc_name)

    def split_command(command: str) -> tuple[str, list[str]]:
        parts = str(command).split()
        return (parts[0] if parts else "echo", parts[1:])

    class MCPClient:  # minimal seam; integration main() needs the real one
        def __init__(self, *args, **kwargs):
            raise NotImplementedError("stub MCPClient")

        async def __aenter__(self):  # pragma: no cover - stub
            raise NotImplementedError

        async def __aexit__(self, *exc):  # pragma: no cover - stub
            return False

    mc.split_command = split_command  # type: ignore[attr-defined]
    mc.MCPClient = MCPClient  # type: ignore[attr-defined]
    sys.modules[mc_name] = mc


_ensure_adapters_importable()

from adapters import xiaohongshu as xhs_module  # noqa: E402
from adapters.xiaohongshu import XiaohongshuAdapter  # noqa: E402

_UNCONFIGURED_LOG = "xiaohongshu 未配置 MCP server，已禁用（fetch 返回空 / publish 抛错）"


class _CaptureHandler(logging.Handler):
    """Collects log records so tests can assert on emitted messages."""

    def __init__(self) -> None:
        super().__init__()
        self.records: list[logging.LogRecord] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.records.append(record)


def _capture_gen():
    handler = _CaptureHandler()
    xhs_module.logger.addHandler(handler)
    yield handler
    xhs_module.logger.removeHandler(handler)


def _unconfigured_gen():
    """Force the adapter into the unconfigured state (mcp_command == 'echo')."""
    orig = xhs_module._load_json_config
    xhs_module._load_json_config = lambda name: {"mcp_command": "echo"}
    try:
        yield XiaohongshuAdapter()
    finally:
        xhs_module._load_json_config = orig


capture = pytest.fixture(_capture_gen)
unconfigured = pytest.fixture(_unconfigured_gen)


def test_unconfigured_emits_info_log(capture, unconfigured):
    assert unconfigured.command == "echo"
    info_messages = [r.getMessage() for r in capture.records if r.levelno == logging.INFO]
    assert any(_UNCONFIGURED_LOG in msg for msg in info_messages)


def test_unconfigured_fetch_returns_empty(capture, unconfigured):
    items = asyncio.run(unconfigured.fetch("AI", 10))
    assert items == []


def test_unconfigured_publish_raises(capture, unconfigured):
    with pytest.raises(RuntimeError):
        asyncio.run(unconfigured.publish("acct", "hello"))


def test_config_template_is_valid_json_with_empty_command():
    example_path = _project_root / "config" / "xiaohongshu.json.example"
    assert example_path.exists(), "config/xiaohongshu.json.example is missing"
    data = json.loads(example_path.read_text(encoding="utf-8"))
    assert data.get("mcp_command") == ""
    assert data.get("fetch_tool")
    assert data.get("publish_tool")


async def main():
    do_publish = "--publish" in sys.argv
    adapter = XiaohongshuAdapter()

    if adapter.command == "echo":
        print("❌ mcp_command 未配置（仍为 'echo' 占位）：")
        print("   请在 apps/pycraper/config/xiaohongshu.json 填写 mcp_command，")
        print("   或设置环境变量 MCP_XHS_COMMAND 后重试。")
        sys.exit(1)

    print(f"🚀 === 小红书 MCP 联调（command={adapter.command!r}）===")

    # 1) 抓取：search_notes
    print("📥 执行 search_notes 抓取 ...")
    try:
        items = await adapter.fetch(keyword="AI", limit=5)
    except Exception as e:
        print(f"❌ 抓取失败: {e}")
        sys.exit(1)
    print(f"✅ 抓取成功，共 {len(items)} 条：")
    for item in items:
        print(f"---\n🔗 {item.url}\n📝 {item.content[:120]}\n")
    if not items:
        print("⚠️ 抓取结果为空（fetch_caches 不会新增记录），请检查 MCP server 登录态/关键词。")

    # 2) 发布：publish_note（仅 --publish 时执行，单次，测试账号）
    if do_publish:
        print("📤 执行 publish_note 发布（测试账号，单次）...")
        try:
            result = await adapter.publish(
                account_id="test",
                content="【联调测试】小红书 MCP 发布链路自测，请忽略。",
            )
        except Exception as e:
            print(f"❌ 发布失败: {e}")
            sys.exit(1)
        print(f"✅ 发布返回: {result}")
        note_id = None
        if isinstance(result, dict):
            note_id = result.get("note_id") or result.get("noteId") or result.get("id")
        if note_id:
            print(f"✅ 确认返回 note id: {note_id}")
        else:
            print("⚠️ 返回中未找到 note id 字段，请人工确认返回内容。")
    else:
        print("（未加 --publish，跳过发布；需要时执行: uv run python tests/test_xiaohongshu.py --publish）")


def _run_unit_tests() -> None:
    """Run the pytest unit tests without pytest (dependency-less sandbox)."""
    cap_gen = _capture_gen()
    capture_handler = next(cap_gen)
    unconf_gen = _unconfigured_gen()
    adapter = next(unconf_gen)
    try:
        assert adapter.command == "echo"
        info_messages = [r.getMessage() for r in capture_handler.records if r.levelno == logging.INFO]
        assert any(_UNCONFIGURED_LOG in msg for msg in info_messages), info_messages
        items = asyncio.run(adapter.fetch("AI", 10))
        assert items == []
        try:
            asyncio.run(adapter.publish("acct", "hello"))
        except RuntimeError:
            pass
        else:
            raise AssertionError("expected RuntimeError for echo publish")
        test_config_template_is_valid_json_with_empty_command()
    finally:
        next(unconf_gen, None)
        next(cap_gen, None)
    print("✅ all xiaohongshu unit tests passed")


if __name__ == "__main__":
    if "--publish" in sys.argv or "--fetch" in sys.argv:
        asyncio.run(main())
    else:
        _run_unit_tests()