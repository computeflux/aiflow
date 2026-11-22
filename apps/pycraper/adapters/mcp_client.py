"""Generic async MCP client using the official Python MCP SDK."""

from __future__ import annotations

import asyncio
import json
import os
import shlex
from typing import Any

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client
from mcp.types import TextContent


def split_command(command: str) -> tuple[str, list[str]]:
    """Split a shell-like command into executable and arguments."""
    parts = shlex.split(command)
    if not parts:
        return "", []
    return parts[0], parts[1:]


def _env_dict(env: dict[str, str] | None) -> dict[str, str]:
    """Return inherited process environment updated with any overrides."""
    base = dict(os.environ)
    if env:
        base.update(env)
    return base


class MCPClient:
    """Wraps an MCP stdio server."""

    def __init__(
        self,
        command: str,
        args: list[str] | None = None,
        env: dict[str, str] | None = None,
    ) -> None:
        self.command = command
        self.args = args or []
        self.env = _env_dict(env)
        self._stdio_cm: Any | None = None
        self._session_cm: Any | None = None
        self._session: ClientSession | None = None

    async def __aenter__(self) -> MCPClient:
        params = StdioServerParameters(
            command=self.command, args=self.args, env=self.env
        )
        self._stdio_cm = stdio_client(params)
        read, write = await self._stdio_cm.__aenter__()
        self._session_cm = ClientSession(read, write)
        self._session = await self._session_cm.__aenter__()
        assert self._session is not None
        await self._session.initialize()
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:  # noqa: ANN001
        if self._session_cm:
            await self._session_cm.__aexit__(exc_type, exc_val, exc_tb)
        if self._stdio_cm:
            await self._stdio_cm.__aexit__(exc_type, exc_val, exc_tb)

    async def list_tools(self) -> list[str]:
        """Return available tool names."""
        assert self._session is not None
        tools = await self._session.list_tools()
        return [t.name for t in tools.tools]

    async def call_tool(
        self, name: str, arguments: dict[str, Any] | None = None
    ) -> Any:
        """Call a tool and return parsed JSON or raw text.

        Raises ``RuntimeError`` with the tool name when the MCP server
        reports an error (``isError``), and bounds the call to 60s.
        """
        assert self._session is not None
        result = await asyncio.wait_for(
            self._session.call_tool(name, arguments=arguments or {}),
            timeout=60,
        )
        if result.isError:
            texts = [c.text for c in result.content if isinstance(c, TextContent)]
            raise RuntimeError(
                f"MCP tool '{name}' failed: {(texts[0] if texts else 'unknown error')[:500]}"
            )
        texts = [c.text for c in result.content if isinstance(c, TextContent)]
        text = "\n".join(texts).strip()
        if not text:
            return None
        try:
            return json.loads(text)
        except json.JSONDecodeError:
            return text
