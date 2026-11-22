"""Xiaohongshu MCP adapter."""

from __future__ import annotations

from typing import Any

from adapters.base import _load_json_config, _read_env_or_config
from adapters.logging import get_logger
from adapters.mcp_client import MCPClient, split_command
from adapters.models import RawItem

logger = get_logger(__name__)


class XiaohongshuAdapter:
    """Fetches and publishes content via the Xiaohongshu MCP server.

    Configuration is loaded from ``config/xiaohongshu.json``; the
    ``MCP_XHS_COMMAND`` env var overrides ``mcp_command`` (env wins,
    same convention as the X adapter's proxy handling).
    """

    def __init__(self) -> None:
        config = _load_json_config("xiaohongshu.json")
        command = _read_env_or_config(config, "mcp_command", "MCP_XHS_COMMAND", "")
        # env 为空/纯空白时回退到 config 中的 mcp_command，而不是硬编码 "echo"；
        # 最终结果 strip，避免 " echo " 这类带空白值绕过 == "echo" 判断后被当作 MCP server 拉起
        self.command = str(command).strip() or str(config.get("mcp_command") or "").strip() or "echo"
        # MK-00 定案：小红书 MCP server = xpzouying/xiaohongshu-mcp，
        # 真实工具名为 search_feeds / publish_content（原假设的 search_notes / publish_note 不存在）
        self.fetch_tool = str(_read_env_or_config(config, "fetch_tool", "MCP_XHS_FETCH_TOOL", "search_feeds"))
        self.publish_tool = str(_read_env_or_config(config, "publish_tool", "MCP_XHS_PUBLISH_TOOL", "publish_content"))
        if self.command == "echo":
            logger.info("xiaohongshu 未配置 MCP server，已禁用（fetch 返回空 / publish 抛错）")

    async def fetch(self, keyword: str, limit: int, mode: str = "") -> list[RawItem]:
        if self.command == "echo":
            logger.debug("xiaohongshu command is 'echo', returning empty")
            return []

        logger.info("fetch xiaohongshu keyword=%r limit=%d", keyword, limit)
        executable, args = split_command(self.command)
        async with MCPClient(executable, args=args) as client:
            result = await client.call_tool(
                self.fetch_tool,
                {"keyword": keyword, "limit": limit},
            )

        if result is None:
            return []
        if isinstance(result, list):
            return [RawItem.from_payload("xiaohongshu", item) for item in result]
        if isinstance(result, dict):
            return [RawItem.from_payload("xiaohongshu", result)]
        return [RawItem(source="xiaohongshu", content=str(result))]

    async def publish(
        self,
        account_id: str,
        content: str,
        title: str = "",
        tags: list[str] | None = None,
        images: list[str] | None = None,
    ) -> dict[str, Any]:
        if self.command == "echo":
            # 与 x.py 一致：MCP 服务未配置时不能假装发布成功，否则 spawn 'echo' 当 MCP server 会卡住/报错
            raise RuntimeError(
                "Xiaohongshu MCP server not configured (mcp_command is 'echo'); "
                "set mcp_command in config/xiaohongshu.json to enable publishing"
            )
        logger.info(
            "publish xiaohongshu account=%s title=%r tags=%d images=%d",
            account_id, title, len(tags or []), len(images or []),
        )
        # 仅透传非空可选参数，保持与旧版 publish_content 实参的向后兼容
        arguments: dict[str, Any] = {"account_id": account_id, "content": content}
        if title:
            arguments["title"] = title
        if tags:
            arguments["tags"] = list(tags)
        if images:
            arguments["images"] = list(images)
        executable, args = split_command(self.command)
        async with MCPClient(executable, args=args) as client:
            return await client.call_tool(self.publish_tool, arguments)