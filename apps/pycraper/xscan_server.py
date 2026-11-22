"""x-scan service 端点：POST /invoke（供 skillgateway 的 invokeService 调用）。

纯 stdlib asyncio 实现（不引入 web 框架依赖）：单路由 POST /invoke，
请求体 JSON 走 run_scan()，响应 JSON 200；任何失败 500 {"ok":false,...}。

trace 传播：读取请求头 traceparent（skillgateway 注入 W3C 格式），
在对应 parent 下开启 span，链路贯穿 api → temporal → go-worker → pycraper。
"""
from __future__ import annotations

import asyncio
import json
import logging
from typing import Any

from opentelemetry import propagate, trace

from skill_entry import run_scan
from telemetry import current_trace_id

logger = logging.getLogger(__name__)

_MAX_BODY = 1 << 20  # 1MB 请求上限


def _extract_ctx(headers: dict[str, str]) -> Any:
    # opentelemetry-api 1.44+ 已移除 HeaderCarrier：dict 直接满足 default getter 协议
    return propagate.extract({k.lower(): v for k, v in headers.items()})


def _http_response(status: int, payload: dict[str, Any]) -> bytes:
    body = json.dumps(payload, ensure_ascii=False, default=str).encode("utf-8")
    reason = {200: "OK", 405: "Method Not Allowed", 404: "Not Found"}.get(status, "Internal Server Error")
    head = (
        f"HTTP/1.1 {status} {reason}\r\n"
        "Content-Type: application/json; charset=utf-8\r\n"
        f"Content-Length: {len(body)}\r\n"
        "Connection: close\r\n\r\n"
    ).encode("ascii")
    return head + body


async def _read_request(reader: asyncio.StreamReader) -> tuple[str, str, dict[str, str], bytes] | None:
    """读一个 HTTP 请求（method/path/headers/body）；连接关闭返回 None。"""
    request_line = await reader.readline()
    if not request_line:
        return None
    parts = request_line.decode("latin-1").split()
    if len(parts) < 2:
        return None
    method, path = parts[0].upper(), parts[1]
    headers: dict[str, str] = {}
    while True:
        line = await reader.readline()
        if not line or line in (b"\r\n", b"\n"):
            break
        if b":" in line:
            k, v = line.decode("latin-1").split(":", 1)
            headers[k.strip()] = v.strip()
    body = b""
    length = int(headers.get("Content-Length", "0") or "0")
    if length > 0:
        body = await reader.read(length)
    return method, path, headers, body


async def _handle(
    reader: asyncio.StreamReader,
    writer: asyncio.StreamWriter,
) -> None:
    try:
        req = await _read_request(reader)
        if req is None:
            return
        method, path, headers, body = req
        if method == "POST" and path.split("?")[0] == "/invoke":
            tracer = trace.get_tracer("xscan")
            ctx = _extract_ctx(headers)
            try:
                input_json: Any = json.loads(body.decode("utf-8") or "{}")
                with tracer.start_as_current_span("x-scan.invoke", context=ctx) as span:
                    span.set_attribute("trace.id", current_trace_id())
                    out = await run_scan(input_json)
            except Exception as exc:
                logger.warning("x-scan invoke failed: %s", exc)
                out = {"ok": False, "reason": str(exc)}
            await writer.write(_http_response(200, out))
        else:
            await writer.write(_http_response(404, {"ok": False, "reason": "not found"}))
    except Exception as exc:
        logger.warning("x-scan http handler error: %s", exc)
        try:
            await writer.write(_http_response(500, {"ok": False, "reason": str(exc)}))
        except Exception:
            pass
    finally:
        try:
            await writer.drain()
            writer.close()
            await writer.wait_closed()
        except Exception:
            pass


async def start_xscan_server(port: int) -> None:
    server = await asyncio.start_server(_handle, "0.0.0.0", port)
    logger.info("x-scan HTTP endpoint listening on :%d (/invoke)", port)
    async with server:
        await server.serve_forever()
