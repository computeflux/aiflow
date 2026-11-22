"""MySQL 连接（共享，惰性创建，进程内缓存）。"""

from __future__ import annotations

import os
from typing import Any

import pymysql

_conn: Any | None = None


def get_mysql() -> Any:
    """获取 MySQL 连接（DictCursor，autocommit）；长空闲断连后自动重连。"""
    global _conn
    if _conn is None:
        _conn = _connect()
    try:
        _conn.ping()
    except Exception:
        try:
            _conn.close()
        except Exception:
            pass
        _conn = _connect()
    return _conn


def _connect() -> Any:
    return pymysql.connect(
        host=os.environ["MYSQL_HOST"],
        port=int(os.environ["MYSQL_PORT"]),
        user=os.environ["MYSQL_USER"],
        password=os.environ["MYSQL_PASSWORD"],
        database=os.environ["MYSQL_DATABASE"],
        charset="utf8mb4",
        autocommit=True,
        cursorclass=pymysql.cursors.DictCursor,
    )
