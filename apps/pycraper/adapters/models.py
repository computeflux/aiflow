"""Shared data models between Go workflow and Python worker."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any

from pydantic import BaseModel, Field, ValidationError


class RawItem(BaseModel):
    """A fetched content item before indexing.

    Mirrors the Go ``db.RawItem`` struct so Temporal can deserialize
    the JSON payload into the correct GORM model.
    """

    source: str
    external_id: str = ""
    keyword: str = ""
    url: str = ""
    content: str
    published_at: datetime | None = None
    fetched_at: datetime = Field(default_factory=lambda: datetime.now(UTC))
    raw_json: str = ""

    @classmethod
    def from_payload(cls, source: str, payload: dict[str, Any]) -> RawItem:
        """Strictly map and validate an MCP tool result payload."""
        # 0. Payload 必须是 dict，非 dict 时给出可读错误
        if not isinstance(payload, dict):
            raise ValueError(
                f"Payload must be a dict, got {type(payload).__name__}: {payload!r}"
            )

        # 1. Basic content validation: content must not be empty
        content = str(payload.get("content") or "").strip()
        if not content:
            raise ValueError(f"Payload missing required 'content' field: {payload}")

        # 2. Extract and sanitize - fallback to URL if no explicit id provided
        external_id = str(payload.get("id") or payload.get("external_id") or "").strip()
        url = str(payload.get("url") or "").strip()
        # 如果 MCP 工具没有提供 id，用 URL 作为去重键（对推文 URL 天然唯一）
        if not external_id and url:
            external_id = url

        # 3. Handle datetime parsing safely
        raw_date = payload.get("published_at") or payload.get("created_at")
        published_at = None
        if raw_date:
            try:
                if isinstance(raw_date, datetime):
                    published_at = raw_date
                else:
                    published_at = datetime.fromisoformat(str(raw_date).replace("Z", "+00:00"))
            except (ValueError, TypeError):
                # Log or handle invalid date gracefully
                published_at = None

        # 4. Construct with validation
        try:
            return cls(
                source=source,
                external_id=external_id,
                url=url,
                content=content,
                published_at=published_at,
                raw_json=json.dumps(payload, ensure_ascii=False, default=str),
            )
        except ValidationError as e:
            raise ValueError(f"Failed to validate RawItem from payload: {e}")
