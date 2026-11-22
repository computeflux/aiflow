"""Tests for XiaohongshuAdapter.publish optional title/tags/images passthrough.

Mocks MCPClient so no real MCP server is spawned, and asserts that empty
optional parameters never appear in the publish_content tool arguments
(backward compatibility with the legacy 2-argument payload).

Note: MK-00 定案小红书 MCP server = xpzouying/xiaohongshu-mcp，发布工具名为
publish_content（原计划假设的 publish_note 不存在）。
"""

from __future__ import annotations

import asyncio
import sys
import types
from pathlib import Path
from typing import Any
from unittest.mock import AsyncMock, MagicMock, patch

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))


def _ensure_adapters_importable() -> None:
    """Make ``adapters`` importable in dependency-less sandboxes (CI contract).

    The CI runner executes these tests with an app .venv; when the venv is
    missing / incomplete, third-party packages pulled in transitively by
    ``adapters.xiaohongshu`` (e.g. the mcp SDK via adapters.mcp_client) are
    not importable. Every publish() test patches
    ``adapters.xiaohongshu.MCPClient`` anyway, so when the real seam cannot
    be imported we install a minimal stub module (same symbols) BEFORE
    importing the adapter, keeping the tests self-contained and hermetic.
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

    class MCPClient:  # minimal seam; tests patch this symbol entirely
        def __init__(self, *args: Any, **kwargs: Any) -> None:
            raise NotImplementedError("stub MCPClient used without a mock")

        async def __aenter__(self):  # pragma: no cover - stub
            raise NotImplementedError

        async def __aexit__(self, *exc):  # pragma: no cover - stub
            return False

    mc.split_command = split_command  # type: ignore[attr-defined]
    mc.MCPClient = MCPClient  # type: ignore[attr-defined]
    sys.modules[mc_name] = mc


_ensure_adapters_importable()

from adapters.xiaohongshu import XiaohongshuAdapter  # noqa: E402


def _make_adapter() -> XiaohongshuAdapter:
    """Build an adapter with a non-echo command so publish() proceeds."""
    adapter = XiaohongshuAdapter()
    adapter.command = "python3 -m fake_xhs_mcp"
    return adapter


def _mock_client(captured: dict[str, Any]) -> MagicMock:
    """Create a mock MCPClient whose call_tool records name + arguments."""

    async def _call_tool(name: str, arguments: dict[str, Any] | None = None) -> Any:
        captured["tool"] = name
        captured["arguments"] = arguments
        return {"ok": True}

    client = MagicMock()
    client.call_tool = AsyncMock(side_effect=_call_tool)
    client.__aenter__ = AsyncMock(return_value=client)
    client.__aexit__ = AsyncMock(return_value=False)
    return client


def _run_publish(**kwargs: Any) -> dict[str, Any]:
    """Run adapter.publish with a mocked MCPClient, return captured call info."""
    adapter = _make_adapter()
    captured: dict[str, Any] = {}
    with patch("adapters.xiaohongshu.MCPClient", return_value=_mock_client(captured)):
        result = asyncio.run(adapter.publish("acct-1", "hello world", **kwargs))
    assert result == {"ok": True}
    return captured


def test_publish_minimal_payload_unchanged() -> None:
    """No optional args → tool arguments must be exactly the legacy 2 keys."""
    captured = _run_publish()
    assert captured["tool"] == "publish_content"
    assert captured["arguments"] == {"account_id": "acct-1", "content": "hello world"}
    assert "title" not in captured["arguments"]
    assert "tags" not in captured["arguments"]
    assert "images" not in captured["arguments"]


def test_publish_empty_optional_params_not_forwarded() -> None:
    """Explicit empty title/tags/images must NOT appear in tool arguments."""
    captured = _run_publish(title="", tags=[], images=[])
    assert captured["arguments"] == {"account_id": "acct-1", "content": "hello world"}
    assert "title" not in captured["arguments"]
    assert "tags" not in captured["arguments"]
    assert "images" not in captured["arguments"]


def test_publish_none_optional_params_not_forwarded() -> None:
    """None optional params must NOT appear in tool arguments."""
    captured = _run_publish(title=None, tags=None, images=None)
    assert captured["arguments"] == {"account_id": "acct-1", "content": "hello world"}
    assert "title" not in captured["arguments"]
    assert "tags" not in captured["arguments"]
    assert "images" not in captured["arguments"]


def test_publish_all_optional_params_forwarded() -> None:
    """Non-empty title/tags/images are passed through verbatim."""
    captured = _run_publish(
        title="我的标题",
        tags=["AI", "ComputeFlux"],
        images=["https://cdn.example.com/a.jpg", "/tmp/b.png"],
    )
    assert captured["arguments"] == {
        "account_id": "acct-1",
        "content": "hello world",
        "title": "我的标题",
        "tags": ["AI", "ComputeFlux"],
        "images": ["https://cdn.example.com/a.jpg", "/tmp/b.png"],
    }


def test_publish_partial_optional_params() -> None:
    """Only non-empty optionals are forwarded; empty ones stay absent."""
    captured = _run_publish(title="仅标题", tags=[], images=["https://cdn.example.com/a.jpg"])
    assert captured["arguments"] == {
        "account_id": "acct-1",
        "content": "hello world",
        "title": "仅标题",
        "images": ["https://cdn.example.com/a.jpg"],
    }
    assert "tags" not in captured["arguments"]


def test_publish_echo_command_still_raises() -> None:
    """Unconfigured (echo) adapter must still fail loudly, not fake success."""
    adapter = XiaohongshuAdapter()
    assert adapter.command == "echo"
    try:
        asyncio.run(adapter.publish("acct-1", "content"))
    except RuntimeError as exc:
        assert "not configured" in str(exc)
    else:
        raise AssertionError("expected RuntimeError for echo command")


if __name__ == "__main__":
    test_publish_minimal_payload_unchanged()
    test_publish_empty_optional_params_not_forwarded()
    test_publish_none_optional_params_not_forwarded()
    test_publish_all_optional_params_forwarded()
    test_publish_partial_optional_params()
    test_publish_echo_command_still_raises()
    print("✅ all xiaohongshu publish tests passed")
