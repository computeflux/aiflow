-- 营销判断服务（docs/marketing/implementation-spec.md §7 / P2 MK-11）：
-- mk_judgments 记录所有 LLM 判断（J1–J18），可回放是硬要求——
-- context_hash + context_snapshot 同表落库，任一条历史判断都能取出
-- 「当时模型看到了什么」+ 模型原始输出。
-- 双轨：本文件给 hacks/bootstrap.sh；internal/marketing/store_judgment.go
-- 的 ensureDDL 给服务自启兜底（两者必须保持一致）。
CREATE TABLE IF NOT EXISTS mk_judgments (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  region VARCHAR(16) NOT NULL,                 -- intl | cn
  product VARCHAR(16) NOT NULL DEFAULT 'network', -- network | shop
  kind VARCHAR(32) NOT NULL,                   -- judge_lead | comment_slot | draft_reply | critique_reply | answer_or_escalate | compliance_check | storefront_copy | analyze_performance
  source VARCHAR(32) NOT NULL,                 -- 判断输入来源（github|reddit|x|zhihu|juejin|v2ex|telegram|discord|manual）
  source_ref VARCHAR(255) DEFAULT '',          -- 来源引用（帖子 URL / 线索 id 等）
  model VARCHAR(64) DEFAULT '',                -- 模型名（回放时确认「哪个模型判的」）
  prompt_version VARCHAR(32) DEFAULT '',       -- prompt 版本（换 prompt 必跑金标回归）
  context_hash CHAR(64) NOT NULL DEFAULT '',   -- 上下文包 hash（回放用，建索引）
  context_snapshot LONGTEXT,                   -- 上下文包完整快照（JSON）
  raw_output LONGTEXT,                         -- 模型原始输出（未解析）
  verdict VARCHAR(32) DEFAULT '',              -- pass | revise | reject | ...
  reason TEXT,                                 -- 判断理由
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  KEY idx_context_hash (context_hash),
  KEY idx_region_kind_created (region, kind, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
