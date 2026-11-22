"""Logging configuration for the scraper worker."""

from __future__ import annotations

import logging
import sys

# Single shared logger for the whole scraper package.
_logger: logging.Logger | None = None


def get_logger(name: str = "scraper") -> logging.Logger:
    """Return (and cache) the scraper logger with a sensible default format.

    Callers can override the level / handlers before any adapter runs by
    calling ``logging.getLogger("scraper")`` directly.
    """
    global _logger
    if _logger is not None:
        return _logger

    _logger = logging.getLogger(name)
    if not _logger.handlers:
        handler = logging.StreamHandler(sys.stderr)
        handler.setFormatter(
            logging.Formatter(
                "%(asctime)s [%(levelname)-5s] %(name)s %(message)s",
                datefmt="%H:%M:%S",
            )
        )
        _logger.addHandler(handler)
        _logger.setLevel(logging.INFO)
        _logger.propagate = False
    return _logger
