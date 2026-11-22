"""Base adapter interface and common config loading."""

from __future__ import annotations

import json
import os
from abc import ABC, abstractmethod
from pathlib import Path
from typing import Any

from adapters.logging import get_logger
from adapters.models import RawItem

logger = get_logger(__name__)


def _load_json_config(name: str) -> dict[str, Any]:
    """Load a JSON config file from the config/ directory.

    Returns an empty dict if the file is missing or unparseable.
    """
    config_path = Path(__file__).resolve().parent.parent / "config" / name
    if not config_path.exists():
        logger.debug("config file not found: %s", config_path)
        return {}
    try:
        return json.loads(config_path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError) as exc:
        logger.warning("failed to load config %s: %s", config_path, exc)
        return {}


def _read_env_or_config(
    config: dict[str, Any],
    key: str,
    env_var: str,
    default: Any = None,
) -> Any:
    """Read a setting from env var first, then JSON config, then fallback to default."""
    value = os.getenv(env_var)
    if value is not None:
        return value
    return config.get(key, default)


class BaseAdapter(ABC):
    """Abstract base for all platform adapters."""

    @abstractmethod
    async def fetch(self, keyword: str, limit: int, mode: str = "") -> list[RawItem]:
        """Fetch raw items from the platform."""

    async def publish(self, account_id: str, content: str) -> dict[str, Any]:
        """Publish content. Override in subclasses that support publishing."""
        raise NotImplementedError(
            f"{self.__class__.__name__} does not support publishing"
        )
