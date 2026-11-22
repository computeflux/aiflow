-- 营销线索 / 动作（docs/marketing/implementation-spec.md §7 / P2 MK-19）：
-- mk_leads  记录各渠道抓到的营销线索（region + product 双维度，idem_key 幂等）；
-- mk_actions 记录对外动作（publish | comment | dm | recruit，idem_key 幂等）。
-- 本阶段（P2）只写不读；发送 / 限流属 M6（规格 §9）。
-- 双轨：本文件给 hacks/bootstrap.sh；internal/marketing/store_lead.go
-- 的 ensureLeadsDDL / ensureActionsDDL 给服务自启兜底（两者必须保持一致）。
CREATE TABLE IF NOT EXISTS mk_leads (
  id           BIGINT AUTO_INCREMENT PRIMARY KEY,
  region       VARCHAR(16) NOT NULL,                 -- intl | cn          ★
  product      VARCHAR(16) NOT NULL DEFAULT 'network', -- network | shop ★
  source       VARCHAR(32) NOT NULL,                 -- github|reddit|x|zhihu|juejin|v2ex|telegram|discord
  source_ref   VARCHAR(255) NOT NULL,
  url          VARCHAR(512) NOT NULL DEFAULT '',
  author       VARCHAR(128) NOT NULL DEFAULT '',
  excerpt      TEXT,
  is_real_need TINYINT NOT NULL DEFAULT 0,
  need_type    VARCHAR(32) NOT NULL DEFAULT 'noise', -- demand|supply|complaint|question|noise（国内无 supply）
  intervene    VARCHAR(16) NOT NULL DEFAULT 'monitor', -- yes|no|monitor
  pain         VARCHAR(32) NOT NULL DEFAULT '',      -- 按 region 分支的枚举（规格 §2.1 J3）
  score        INT NOT NULL DEFAULT 0,
  confidence   DECIMAL(4,3) NOT NULL DEFAULT 0,
  judgment_id  BIGINT NOT NULL DEFAULT 0,
  idem_key     VARCHAR(128) NOT NULL,
  status       VARCHAR(32) NOT NULL DEFAULT 'new',
  found_at     DATETIME NOT NULL,
  UNIQUE KEY uk_idem (idem_key),
  KEY idx_region_status (region, status, score DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS mk_actions (
  id                     BIGINT AUTO_INCREMENT PRIMARY KEY,
  region                 VARCHAR(16) NOT NULL,        -- intl | cn          ★
  product                VARCHAR(16) NOT NULL DEFAULT 'network', -- network | shop ★
  lead_id                BIGINT NOT NULL DEFAULT 0,
  agent                  VARCHAR(8) NOT NULL,         -- a1..a6
  kind                   VARCHAR(16) NOT NULL,        -- publish|comment|dm|recruit（comment 含回复/答疑/截流，见 §1.5）
  channel                VARCHAR(48) NOT NULL,        -- github|x|telegram|wechat-mp|manual-zhihu...
  account                VARCHAR(64) NOT NULL,
  target_url             VARCHAR(512) NOT NULL DEFAULT '',
  body                   LONGTEXT NOT NULL,
  draft_judgment_id      BIGINT NOT NULL DEFAULT 0,
  critique_judgment_id   BIGINT NOT NULL DEFAULT 0,
  compliance_judgment_id BIGINT NOT NULL DEFAULT 0,
  status                 VARCHAR(32) NOT NULL DEFAULT 'draft',
  idem_key               VARCHAR(128) NOT NULL,
  created_at             DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_idem (idem_key),
  KEY idx_region_status (region, status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
