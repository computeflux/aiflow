"""GitHub adapter: scrape public commits from an org's repos."""

from __future__ import annotations

import asyncio
import json
import shutil
import subprocess
import tempfile
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import requests
from temporalio import activity

from adapters.base import _load_json_config, _read_env_or_config
from adapters.logging import get_logger
from adapters.models import RawItem

logger = get_logger(__name__)

# Sentinel values for subprocess timeout (10 minutes).
_CLONE_TIMEOUT_SEC = 600
_LOG_TIMEOUT_SEC = 120
# 每个仓库最多解析的 commit 条数（git log -n）
_GIT_LOG_LIMIT = 200
# 并发 clone 的上限
_CLONE_CONCURRENCY = 3


def _heartbeat(detail: str = "") -> None:
    """发送 Temporal activity 心跳；非 activity 上下文或失败时静默忽略。"""
    try:
        activity.heartbeat(detail)
    except Exception:
        pass


class GitHubAdapter:
    """Fetches public commits from a GitHub organization's repositories.

    Configuration is loaded from ``config/github.json``, with overrides
    from environment variables:

    * ``GITHUB_TOKEN`` – Personal Access Token (env-only, never in JSON).
    """

    def __init__(self) -> None:
        config = _load_json_config("github.json")

        # Token is env-only for security.
        self.token = _read_env_or_config(config, "token", "GITHUB_TOKEN", "")
        self.git_bin = (
            _read_env_or_config(config, "git_bin_path", "GIT_BIN_PATH")
            or shutil.which("git")
            or "git"
        )
        self.api_base = _read_env_or_config(
            config, "api_base", "GITHUB_API_BASE", "https://api.github.com"
        )

        if not self.token:
            logger.warning(
                "GITHUB_TOKEN is not set – GitHub API requests will be rate-limited "
                "(60 req/h for unauthenticated calls)."
            )

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _headers(self) -> dict[str, str]:
        headers: dict[str, str] = {"Accept": "application/vnd.github+json"}
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        return headers

    def _list_repos(self, org: str) -> list[dict[str, Any]]:
        """Paginate through an org's public source repos, newest first."""
        url = f"{self.api_base}/orgs/{org}/repos"
        repos: list[dict[str, Any]] = []
        page = 1
        # 分页上限：防止代理/网关忽略 page 参数导致死循环
        max_pages = 20

        logger.info("listing repos for org=%s", org)
        while page <= max_pages:
            logger.debug("_list_repos page=%d", page)
            resp = requests.get(
                url,
                headers=self._headers(),
                params={
                    "type": "sources",
                    "sort": "pushed",
                    "direction": "desc",
                    "per_page": 100,
                    "page": page,
                },
                timeout=30,
            )
            # 403 + X-RateLimit-Remaining=0 = 限流（匿名 60 req/h 很容易被打光）。
            # 读 X-RateLimit-Reset 等配额恢复后重试；等待期间发心跳防 activity 超时。
            if (
                resp.status_code == 403
                and resp.headers.get("x-ratelimit-remaining") == "0"
            ):
                reset_at = float(resp.headers.get("x-ratelimit-reset", 0) or 0)
                wait = min(max(reset_at - time.time(), 0), 3600)
                logger.warning(
                    "GitHub API rate limited; waiting %.0f s until quota resets",
                    wait,
                )
                deadline = time.monotonic() + wait
                while time.monotonic() < deadline:
                    _heartbeat("waiting for GitHub rate limit reset")
                    time.sleep(min(30, max(deadline - time.monotonic(), 1)))
                resp = requests.get(
                    url,
                    headers=self._headers(),
                    params={
                        "type": "sources",
                        "sort": "pushed",
                        "direction": "desc",
                        "per_page": 100,
                        "page": page,
                    },
                    timeout=30,
                )
            # 401 = token 无效（如容器里配了非 GitHub 的 token）。
            # 公开 org 无需鉴权即可列出，降级为匿名重试一次。
            if resp.status_code == 401 and self.token:
                logger.warning(
                    "GITHUB_TOKEN rejected by GitHub API (401); "
                    "falling back to unauthenticated requests"
                )
                self.token = ""
                resp = requests.get(url, headers=self._headers(), params={
                    "type": "sources",
                    "sort": "pushed",
                    "direction": "desc",
                    "per_page": 100,
                    "page": page,
                }, timeout=30)
            resp.raise_for_status()
            data = resp.json()
            if not isinstance(data, list) or not data:
                break
            repos.extend(data)
            page += 1

        if page > max_pages:
            logger.warning(
                "_list_repos hit max_pages=%d for org=%s; "
                "pagination may be looping (proxy ignoring page param?)",
                max_pages, org,
            )

        logger.info("found %d repository(s) for %s", len(repos), org)
        return repos

    def _clone_and_log(self, clone_url: str, repo_name: str) -> list[dict[str, str]]:
        """Shallow-clone a repo, parse ``git log``, return deduplicated commits."""
        commits: list[dict[str, str]] = []
        tmpdir: str | None = None
        logger.info("cloning %s -> %s", repo_name, clone_url)

        try:
            tmpdir = tempfile.mkdtemp(prefix="github_")
            repo_dir = Path(tmpdir) / repo_name
            subprocess.run(
                [
                    self.git_bin,
                    "clone",
                    "--no-single-branch",
                    clone_url,
                    str(repo_dir),
                ],
                check=True,
                timeout=_CLONE_TIMEOUT_SEC,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )

            fmt = "%H%x1f%an%x1f%ae%x1f%aI%x1f%s"
            proc = subprocess.run(
                [
                    self.git_bin,
                    "-C",
                    str(repo_dir),
                    "log",
                    f"--pretty=format:{fmt}%x1e",
                    "--no-merges",
                    "-n",
                    str(_GIT_LOG_LIMIT),
                ],
                check=True,
                timeout=_LOG_TIMEOUT_SEC,
                capture_output=True,
                text=True,
            )

            seen_hashes: set[str] = set()
            for raw in proc.stdout.split("\x1e"):
                raw = raw.strip()
                if not raw:
                    continue
                parts = raw.split("\x1f")
                if len(parts) < 5:
                    continue

                commit_hash = parts[0]
                email = parts[2]
                if "noreply.github.com" in email:
                    continue
                if commit_hash in seen_hashes:
                    continue
                seen_hashes.add(commit_hash)

                commits.append(
                    {
                        "repo": repo_name,
                        "hash": parts[0],
                        "author": parts[1],
                        "email": email,
                        "date": parts[3],
                        "message": parts[4].strip(),
                    }
                )

            logger.info(
                "%s: %d commits (after dedup + noreply filter)",
                repo_name,
                len(commits),
            )
        except subprocess.TimeoutExpired as exc:
            logger.error("timeout cloning/parsing %s: %s", repo_name, exc)
        except subprocess.CalledProcessError as exc:
            logger.error("git failed for %s: %s", repo_name, exc)
        finally:
            if tmpdir:
                shutil.rmtree(tmpdir, ignore_errors=True)

        return commits

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def fetch(self, org: str, limit: int = 0, mode: str = "") -> list[RawItem]:
        """Fetch commits across an org's top repos (``limit`` is ignored)."""
        logger.info("fetch github org=%s", org)
        repos = await asyncio.to_thread(self._list_repos, org)
        if not repos:
            logger.warning("no repos found for org=%s", org)
            return []

        logger.info("cloning %d repository(s) concurrently (max %d at a time)", len(repos), _CLONE_CONCURRENCY)
        sem = asyncio.Semaphore(_CLONE_CONCURRENCY)

        async def _limited_clone(clone_url: str, name: str):
            async with sem:
                _heartbeat(f"cloning {name}")
                return await asyncio.to_thread(self._clone_and_log, clone_url, name)

        tasks: list[tuple[str, asyncio.Task]] = []
        for r in repos:
            if not r.get("clone_url"):
                continue
            name = r.get("full_name", r.get("name", "unknown"))
            tasks.append((name, asyncio.create_task(_limited_clone(r["clone_url"], name))))

        # return_exceptions=True：单个仓库失败不影响其他仓库
        gathered = await asyncio.gather(*(t for _, t in tasks), return_exceptions=True)

        items: list[RawItem] = []
        for (name, _), res in zip(tasks, gathered):
            if isinstance(res, BaseException):
                logger.error("clone/log failed for %s: %s", name, res)
                continue
            commits = res
            for commit in commits:
                published_at = self._parse_iso(commit.get("date", ""))
                url = f"https://github.com/{commit['repo']}/commit/{commit['hash']}"
                content = (
                    f"repo: {commit['repo']}\n"
                    f"author: {commit['author']} <{commit['email']}>\n"
                    f"message: {commit['message']}"
                )
                items.append(
                    RawItem(
                        source="github",
                        external_id=f"github:{commit['repo']}:{commit['hash']}",
                        url=url,
                        content=content,
                        published_at=published_at,
                        raw_json=json.dumps(commit, ensure_ascii=False),
                    )
                )

        logger.info("github fetch complete: %d raw items", len(items))
        return items

    @staticmethod
    def _parse_iso(value: str) -> datetime | None:
        if not value:
            return None
        try:
            dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=UTC)
            return dt
        except ValueError:
            return None
