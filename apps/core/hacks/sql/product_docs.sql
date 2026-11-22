-- S7 产品情报（docs/plan/S7-产品情报.md §3.2）：
-- 产品 Agent 生成的文档 / 变更需求（product 角色，业务智能体，不写代码）。
-- doc_type：product_doc | change_request | meeting_minutes；
-- sources 为可溯源来源（RAG 命中文档列表，含时间戳）；card_id 为拆解后回填的任务卡。
CREATE TABLE IF NOT EXISTS product_docs (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  board_id BIGINT NOT NULL,
  title VARCHAR(255) NOT NULL,
  doc_type VARCHAR(32) NOT NULL,         -- product_doc | change_request | meeting_minutes
  content LONGTEXT NOT NULL,
  sources JSON,                          -- 可溯源来源（RAG 命中文档列表）
  card_id BIGINT,                        -- 关联任务卡（拆解后回填）
  status VARCHAR(32) NOT NULL DEFAULT 'draft',  -- draft | accepted | rejected
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
