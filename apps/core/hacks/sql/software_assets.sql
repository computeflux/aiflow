-- S5 软件资产画像（docs/plan/S5-软件接管与部署编排.md §3.1）：
-- deployctl inventory / cmd/inventory 扫描仓库生成；
-- auto_managed=1 的资产纳入部署编排（deploy all）与健康巡检自愈（health-patrol）；
-- start_method=external 的资产（contract-deploy / tee-node-monitor / postiz / 业务 dApp）
-- 在 deploy all / 自愈 / 版本管理中零触碰（红线，与 transform-plan §1.1 一致）。
CREATE TABLE IF NOT EXISTS software_assets (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL UNIQUE,
  path VARCHAR(255) NOT NULL,
  kind VARCHAR(32) NOT NULL,               -- go-service|python-worker|vue-app|react-app|docker-app|cli
  language VARCHAR(32),
  ports JSON,
  start_method VARCHAR(32) NOT NULL,       -- compose | dynamic（版本容器） | external（不纳管）
  service VARCHAR(128),                    -- compose 服务名（一服务一文件）
  interfaces JSON,
  owner VARCHAR(64) NOT NULL,
  profile JSON NOT NULL,                   -- 完整画像快照（含依赖树摘要）
  auto_managed TINYINT(1) NOT NULL DEFAULT 0,  -- 是否纳入部署编排与自愈
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
