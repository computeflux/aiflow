-- S7 产品情报（docs/plan/S7-产品情报.md §3.1）：
-- knowledge-sync 增量入库的去重基线：按文件路径 + 内容 hash 做 diff，
-- 内容未变则跳过（避免重复向量化），变更则重新入库并刷新 ingested_at。
-- 已入库的 Qdrant 数据不随本表删除而删除（§7 回滚：stop knowledge-sync 即止，数据保留）。
CREATE TABLE IF NOT EXISTS knowledge_docs (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  path VARCHAR(512) NOT NULL UNIQUE,
  content_hash CHAR(64) NOT NULL,
  source_type VARCHAR(32) NOT NULL,
  confidentiality VARCHAR(16) NOT NULL DEFAULT 'internal',
  title VARCHAR(255) NOT NULL,
  ingested_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
