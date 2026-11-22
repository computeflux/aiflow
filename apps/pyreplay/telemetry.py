"""OpenTelemetry 链路追踪：初始化 + traced 装饰器 + trace id 提取。

用法：
    from telemetry import init_tracing, traced, current_trace_id

    init_tracing("pycraper")          # main() 启动时调用一次（幂等）

    @traced("FetchActivities.fetch")  # 给函数/方法开启 span
    async def fetch(...): ...

OTEL_EXPORTER_OTLP_ENDPOINT 默认 http://otel-collector:4317（容器内网络，
经 otel-collector 转发到 Jaeger）。
"""

from __future__ import annotations

import atexit
import functools
import inspect
import logging
import os
from typing import Any, Callable, Optional

from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace import Status, StatusCode

logger = logging.getLogger(__name__)

_provider: Optional[TracerProvider] = None


def init_tracing(service_name: str) -> None:
    """初始化全局 TracerProvider 并注册 atexit flush。幂等：重复调用不重建。"""
    global _provider
    if _provider is not None:
        return
    endpoint = os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4317")
    exporter = OTLPSpanExporter(endpoint=endpoint, insecure=True)
    provider = TracerProvider(
        resource=Resource.create({"service.name": service_name}),
    )
    provider.add_span_processor(BatchSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    _provider = provider
    atexit.register(provider.shutdown)
    logger.info("otel tracing initialized: service=%s endpoint=%s", service_name, endpoint)


def traced(name: Optional[str] = None, **attrs: Any) -> Callable:
    """给函数/方法开启一个 span（默认取函数名），异常时记录错误状态。

    同时支持 async 与普通函数；装饰后签名与行为不变。
    """

    def deco(fn: Callable) -> Callable:
        span_name = name or fn.__name__

        @functools.wraps(fn)
        async def async_wrapper(*args: Any, **kwargs: Any) -> Any:
            tracer = trace.get_tracer("telemetry")
            with tracer.start_as_current_span(span_name) as span:
                if attrs:
                    span.set_attributes({k: str(v) for k, v in attrs.items()})
                try:
                    return await fn(*args, **kwargs)
                except Exception as exc:
                    span.set_status(Status(StatusCode.ERROR, str(exc)[:1024]))
                    span.record_exception(exc)
                    raise

        @functools.wraps(fn)
        def sync_wrapper(*args: Any, **kwargs: Any) -> Any:
            tracer = trace.get_tracer("telemetry")
            with tracer.start_as_current_span(span_name) as span:
                if attrs:
                    span.set_attributes({k: str(v) for k, v in attrs.items()})
                try:
                    return fn(*args, **kwargs)
                except Exception as exc:
                    span.set_status(Status(StatusCode.ERROR, str(exc)[:1024]))
                    span.record_exception(exc)
                    raise

        if inspect.iscoroutinefunction(fn):
            return async_wrapper
        return sync_wrapper

    return deco


def current_trace_id() -> str:
    """返回当前 span 的 trace id（32 位 hex），无有效 span 时返回空串。

    用于写入 error_logs.trace_id，打通错误与链路。
    """
    ctx = trace.get_current_span().get_span_context()
    if ctx is None or not ctx.is_valid:
        return ""
    return format(ctx.trace_id, "032x")
