"""Shared data models between Go workflow and Python worker."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any

from pydantic import BaseModel, Field


class LikeTarget(BaseModel):
    """A tweet/post to be liked."""

    tweet_id: str
    tweet_url: str = ""
    author: str = ""
    content: str = ""


class LikeResult(BaseModel):
    """Result of a like operation."""

    tweet_id: str
    tweet_url: str = ""
    success: bool
    error: str = ""
    liked_at: datetime = Field(default_factory=lambda: datetime.now(UTC))


class XAccount(BaseModel):
    """A logged-in X account (cookies) available for liking."""

    id: int
    username: str = ""
    auth_token: str = ""
    ct0: str = ""
    enabled: bool = True
    daily_like_limit: int = 50
    cooldown_minutes: int = 10
    used_today: int = 0
    last_used_at: datetime | None = None
