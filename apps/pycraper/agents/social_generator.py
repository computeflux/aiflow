"""Generate Twitter posts from top-ranked recent content."""

from __future__ import annotations

import json
import os
import re as _re
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pymysql
import requests
from dotenv import load_dotenv

from agents.ranker import score_item, theme_keywords

load_dotenv(Path(__file__).resolve().parent.parent / ".env")

_INTENT_PATTERNS = [
    "how to", "help", "question", "anyone", "why", "error", "stuck",
    "wish", "please", "need", "looking for", "any idea", "any tips",
    "recommend", "?",
]

_THEME_QUERIES = [
    "AI compute", "TEE", "privacy", "decentralized", "GPU",
    "inference", "crypto", "blockchain", "Web3", "smart contract",
    "open source", "anonymous", "verifiable",
]

_core_path = Path(__file__).resolve().parent.parent.parent.parent / "docs" / "core.md"
CORPUS_MEMORY = _core_path.read_text(encoding="utf-8").strip() if _core_path.exists() else \
    "You are ComputeFlux, a decentralized AI infrastructure network."

SYSTEM_PROMPT = f"""You are ComputeFlux's social media persona.  Your tweets are posted to X.

## Brand context
{CORPUS_MEMORY}

## Input
Each request provides:
- A news feed (tech/crypto/AI posts sorted by engagement 🔥)
- Recently posted tweets (to avoid repetition)

## Task
Write ONE tweet reacting to a specific item in the feed.
Output ONLY valid JSON — the `url` field is MANDATORY:

{{"url": "https://...", "tweet": "..."}}

## Voice
- Every tweet MUST name-drop ComputeFlux — explain why this news matters to us
- Hot take: say something people will disagree with. Start a fight.
- "Actually, the real problem is..." or "Everyone's excited about X but missing Y"
- Ask a provocative question that forces a reply
- Specific insight > generic take; contrarian > agreeable
- 3-6 sentences, 1-2 hashtags woven naturally

## Anti-patterns (never do these)
- Generic observations, "Check this out", "Excited to announce", "We believe"
- ALL CAPS, emoji abuse, marketing jargon
- Repeating a topic we already posted
- Forcing ComputeFlux into an unrelated topic
"""


@dataclass
class SocialGenerateInput:
    hours: int = 24
    count: int = 5
    source: str = "x"


def _get_mysql():
    return pymysql.connect(
        host=os.environ["MYSQL_HOST"], port=int(os.environ["MYSQL_PORT"]),
        user=os.environ["MYSQL_USER"], password=os.environ["MYSQL_PASSWORD"],
        database=os.environ["MYSQL_DATABASE"], charset="utf8mb4", autocommit=True,
        connect_timeout=5, read_timeout=30, write_timeout=30,
    )


# ---------------------------------------------------------------------------
# main entry point
# ---------------------------------------------------------------------------

async def generate_social_content(input: SocialGenerateInput) -> dict:
    # count 钳制：LLM 调用按条数放大成本，防止配置/调用方传过大数值
    count = max(1, min(int(input.count or 5), 20))

    conn = _get_mysql()
    try:
        keywords = theme_keywords(_THEME_QUERIES)

        sources = [s.strip() for s in input.source.split(",") if s.strip()]
        if not sources:
            # 空 source 会生成非法 SQL `WHERE source IN ()`，退化为默认来源
            print("[social] WARNING: empty source list, defaulting to ['x']")
            sources = ["x"]
        placeholders = ",".join(["%s"] * len(sources))
        cutoff = datetime.now(timezone.utc).replace(tzinfo=None) - timedelta(hours=input.hours)

        sql = (
            f"SELECT id, source, external_id, keyword, url, content, "
            f"published_at, fetched_at, raw_json "
            f"FROM raw_items WHERE source IN ({placeholders}) AND fetched_at >= %s "
            f"ORDER BY fetched_at DESC LIMIT 200"
        )
        with conn.cursor(pymysql.cursors.DictCursor) as cur:
            cur.execute(sql, (*sources, cutoff))
            rows = cur.fetchall()

        print(f"[social] fetched {len(rows)} items from last {input.hours}h")

        scored = []
        for row in rows:
            s, reasons = score_item(item=_row_to_raw_item(row), keywords=keywords, intent_patterns=_INTENT_PATTERNS)
            eng = _parse_engagement(row)
            scored.append((s + eng * 0.01, row, reasons))
        scored.sort(key=lambda x: x[0], reverse=True)

        top_items = scored[:50]
        news_summary = "\n".join(
            f"- [{row['source'].upper()}] 🔥{_parse_engagement(row)} {_extract_text(row)}  {row.get('url','')}"
            for _, row, _ in top_items
        )[:12000]

        # recent tweets for continuity
        own_sql = (
            "SELECT content FROM raw_items WHERE keyword = 'computefluxAI' AND source = 'x' "
            "ORDER BY published_at DESC LIMIT 10"
        )
        with conn.cursor(pymysql.cursors.DictCursor) as cur:
            cur.execute(own_sql)
            own_rows = cur.fetchall()
        if own_rows:
            own_summary = "\n".join(f"- {_extract_text(r)}" for r in own_rows)[:2000]
            news_summary = f"## Recently posted (DO NOT REPEAT)\n{own_summary}\n\n## News feed\n{news_summary}"

        api_key = os.environ["OPENAI_API_KEY"]
        base_url = os.environ["OPENAI_BASE_URL"]
        model = os.environ.get("LLM_SOCIAL_MODEL") or os.environ["LLM_MODEL"]

        tweets = []
        for i in range(count):
            result = await _gen(news_summary, i, api_key, base_url, model)
            ok = "OK" if result else "EMPTY"
            print(f"[social] tweet {i}: {ok}")
            if result:
                url, tweet, reasoning = result
                if not tweet.strip():
                    print(f"[social] tweet {i}: SKIP (empty content)")
                    continue

                print({"score": 0, "source_url": url, "keyword": "trend", "tweet": tweet, "reasoning": reasoning})
                tweets.append({"score": 0, "source_url": url, "keyword": "trend", "tweet": tweet, "reasoning": reasoning, "reasons": []})

        return {"tweets": tweets, "total_scored": len(scored)}
    finally:
        try:
            conn.close()
        except Exception:
            pass


# ---------------------------------------------------------------------------
# tweet generation
# ---------------------------------------------------------------------------

async def _gen(
    news_summary: str, index: int, api_key: str, base_url: str, model: str,
) -> tuple[str, str, str] | None:
    prompt = f"{news_summary}\n\nWrite tweet #{index + 1}. Output JSON only."

    try:
        resp = requests.post(
            f"{base_url}/chat/completions",
            headers={"Authorization": f"Bearer {api_key}"},
            json={
                "model": model,
                "messages": [
                    {"role": "system", "content": SYSTEM_PROMPT},
                    {"role": "user", "content": prompt},
                ],
                "max_tokens": 4096,
                "temperature": 0.85,
                "response_format": {"type": "json_object"},
            },
            timeout=60,
        )
        resp.raise_for_status()
        data = resp.json()
        reasoning = _extract_reasoning(data)
        content = (data.get("choices", [{}])[0].get("message", {}).get("content") or "").strip()

        result = _try_json(content)
        if result:
            url, tweet = result
            return url, tweet, reasoning

        print(f"[social] WARNING: JSON parse failed")
        return None

    except Exception as e:
        print(f"[social] ERROR tweet {index}: {e}")
        return None


def _extract_reasoning(data: dict) -> str:
    """Extract thinking/reasoning from LLM response for UI display."""
    choice = (data.get("choices") or [{}])[0]
    msg = choice.get("message", {}) if isinstance(choice, dict) else {}

    # DeepSeek-style reasoning_content
    rc = (msg.get("reasoning_content") or "").strip()
    if rc:
        return rc[:5000]

    # OpenAI o1 / o3 / GPT-5 style: content in choice directly
    # (some models put reasoning in a separate field or prefix)
    reasoning_keys = ("reasoning", "thinking", "chain_of_thought", "cot")
    for key in reasoning_keys:
        val = msg.get(key)
        if val and str(val).strip():
            return str(val).strip()[:5000]

    # Check if there's a top-level reasoning field (some proxies add this)
    for key in reasoning_keys:
        val = data.get(key)
        if val and str(val).strip():
            return str(val).strip()[:5000]

    return ""


def _try_json(text: str) -> tuple[str, str] | None:
    """Try parsing JSON output: {"url": "...", "tweet": "..."}"""
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        m = _re.search(r"\{.*\}", text, _re.DOTALL)
        if not m:
            return None
        try:
            data = json.loads(m.group(0))
        except json.JSONDecodeError:
            return None
    url = data.get("url") or data.get("source_url", "")
    tweet = data.get("tweet") or data.get("text") or data.get("content", "")
    if tweet:
        return url, tweet
    return None

# ---------------------------------------------------------------------------
# data helpers
# ---------------------------------------------------------------------------

def _extract_text(row: dict) -> str:
    if row.get("raw_json"):
        try:
            data = json.loads(row["raw_json"])
            text = data.get("text") or data.get("title", "")
            if text:
                return str(text)[:300]
        except (json.JSONDecodeError, TypeError):
            pass
    for line in (row.get("content", "")).split("\n"):
        line = line.strip()
        if len(line) > 10:
            return line[:300]
    return (row.get("content", ""))[:300]


def _parse_engagement(row: dict) -> int:
    if row.get("raw_json"):
        try:
            data = json.loads(row["raw_json"])
            v, l, r = int(data.get("views", 0)), int(data.get("likes", 0)), int(data.get("retweets", 0))
            if v + l + r > 0:
                return v + l * 2 + r * 3
        except (json.JSONDecodeError, TypeError, ValueError):
            pass
    content = row.get("content", "")
    vm = _re.search(r"👁\s*(\d+)", content)
    rm = _re.search(r"🔄\s*(\d+)", content)
    lm = _re.search(r"👍\s*(\d+)", content)
    views = int(vm.group(1)) if vm else 0
    retweets = int(rm.group(1)) if rm else 0
    likes = int(lm.group(1)) if lm else 0
    return views + likes * 2 + retweets * 3


def _row_to_raw_item(row: dict):
    from adapters.models import RawItem
    return RawItem(
        source=row.get("source", ""),
        content=row.get("content", ""),
        published_at=row.get("published_at"),
        raw_json=row.get("raw_json") or "{}",
    )
