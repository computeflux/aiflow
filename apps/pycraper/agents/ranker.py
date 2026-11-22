"""按 ComputeFlux 推文搜索原则给推文打分。

规则来源:docs/marketing/scraper/x.md
加分:
- 提问/抱怨类意图词(intent_patterns)每命中一个 +1.5
  (匹配前先剔除 URL,避免链接查询串里的 "?" 被误判为提问)
- 越新越好:24 小时内线性加分,最多 +3(时间未知不加不减)
- 主题相关性:正文包含主题查询里的关键词,每个 +0.5,上限 +2
减分(找的是有需求的人,不是项目方广告/喊单/水军):
- 外链 >= 2 个 -1.5(推销贴特征)
- hashtag >= 2 个 -1.0
- cashtag($LNQ、$META 这类代币/股票代码)-2.0(喊单贴特征,
  真正有需求的开发者几乎不用 cashtag)
- 推销句式(is live on / still early / get in 等)-1.0/个,最多 -2
- 垃圾词(airdrop/giveaway/hiring/jobs)-3
- 正文过短(< 40 字符,信息量低;不含作者前缀)-1
- 短且无信号(< 100 字符且未命中任何意图词)-1:口号式水贴的典型形态;
  真正的短提问有 "?" 意图加分兜底,不受影响

所有文本判断基于推文正文(优先从 raw_json 取 text 字段),
不含作者名,避免作者昵称/handle 干扰匹配。
"""

from __future__ import annotations

import json
import re
from datetime import datetime, timezone

from adapters.models import RawItem

EXCLUDE_WORDS = ["airdrop", "giveaway", "hiring", "jobs"]

PROMO_PHRASES = [
    "is live on",
    "just launched",
    "launching",
    "we're excited",
    "dms open",
    "check out",
    "join us",
    "don't miss",
    "positioned at",
    "stay tuned",
    "still early",
    "get in",
    "don't sleep on",
    "worth paying attention",
]

# 代币/股票代码,如 $LNQ、$META(纯数字金额 $2.99 不算)
_CASHTAG_RE = re.compile(r"\$[A-Za-z]{2,6}\b")

# 抓下来的推文正文里 URL 常被空格打断(如 "https:// lvlltd.com/..."),
# 所以 scheme 后允许一个空格;裸域名(basescan.org/tx/... 等)也要剔除。
_URL_RE = re.compile(
    r"(?:https?://\s?\S+)"
    r"|(?:\b[\w-]+(?:\.[\w-]+)*\.(?:com|net|org|io|ai|xyz|app|dev|json)\b(?:/\S*)?)",
    re.IGNORECASE,
)

# 主题查询词里无区分度的常用词,不参与相关性计分
_STOPWORDS = {"too", "for", "the", "and", "with", "api", "out"}


def theme_keywords(theme_queries: list[str]) -> list[str]:
    """从主题查询词组里抽出有区分度的关键词(小写)。"""
    words: set[str] = set()
    for q in theme_queries:
        for w in re.findall(r"[a-zA-Z][a-zA-Z0-9-]{3,}", q.lower()):
            if w not in _STOPWORDS:
                words.add(w)
    return sorted(words)


def _tweet_text(item: RawItem) -> str:
    """取推文正文(不含作者前缀)。优先 raw_json 的 text 字段。"""
    if item.raw_json:
        try:
            data = json.loads(item.raw_json)
            text = data.get("text")
            if text:
                return str(text)
        except (json.JSONDecodeError, TypeError):
            pass
    _, sep, rest = item.content.partition(": ")
    return rest if sep else item.content


def _matches_intent(pattern: str, text: str) -> bool:
    if pattern == "?":
        # 只认词尾的问号(真提问);URL 查询串/代码里的 ?key= 后面紧跟字符,不算
        return re.search(r"\?[\"')\]]*(?:\s|$)", text) is not None
    return pattern in text


def score_item(
    item: RawItem,
    keywords: list[str],
    intent_patterns: list[str],
    now: datetime | None = None,
) -> tuple[float, list[str]]:
    """返回 (分数, 加减分原因列表)。"""
    now = now or datetime.now(timezone.utc)
    text = _tweet_text(item)
    text_no_url = _URL_RE.sub(" ", text).lower()
    score = 0.0
    reasons: list[str] = []

    matched = [p for p in intent_patterns if _matches_intent(p, text_no_url)]
    if matched:
        pts = 1.5 * len(matched)
        score += pts
        reasons.append(f"意图词 {','.join(matched)} +{pts:.1f}")

    ts = item.published_at
    if ts is not None:
        if ts.tzinfo is None:
            ts = ts.replace(tzinfo=timezone.utc)
        age_h = (now - ts).total_seconds() / 3600
        if 0 <= age_h <= 24:
            pts = 3.0 * (24 - age_h) / 24
            score += pts
            reasons.append(f"发布于 {age_h:.1f}h 前 +{pts:.1f}")

    hits = [w for w in keywords if w in text_no_url]
    if hits:
        pts = min(0.5 * len(hits), 2.0)
        score += pts
        reasons.append(f"主题词 {','.join(hits[:4])} +{pts:.1f}")

    links = len(_URL_RE.findall(text))
    if links >= 2:
        score -= 1.5
        reasons.append(f"外链 {links} 个 -1.5")

    hashtags = len(re.findall(r"#\w+", text_no_url))
    if hashtags >= 2:
        score -= 1.0
        reasons.append(f"hashtag {hashtags} 个 -1.0")

    cashtags = _CASHTAG_RE.findall(text)
    if cashtags:
        score -= 2.0
        reasons.append(f"cashtag {','.join(cashtags[:3])} -2.0")

    promo = [p for p in PROMO_PHRASES if p in text_no_url]
    if promo:
        pts = min(1.0 * len(promo), 2.0)
        score -= pts
        reasons.append(f"推销句式 {','.join(promo)} -{pts:.1f}")

    spam = [w for w in EXCLUDE_WORDS if w in text_no_url]
    if spam:
        score -= 3.0
        reasons.append(f"垃圾词 {','.join(spam)} -3.0")

    if len(text) < 40:
        score -= 1.0
        reasons.append("内容过短 -1.0")

    if not matched and len(text) < 100:
        score -= 1.0
        reasons.append("短且无意图信号 -1.0")

    return score, reasons
