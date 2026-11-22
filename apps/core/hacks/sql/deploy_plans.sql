-- S5 部署计划（docs/plan/S5-软件接管与部署编排.md §3.2）：
-- deployctl plan [--save] 生成依赖序部署计划（拓扑排序）；
-- deploy 每步执行结果写 last_result（含每步耗时，可回溯）。
CREATE TABLE IF NOT EXISTS deploy_plans (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL,
  plan JSON NOT NULL,
  -- [{target, kind: build|start|check, depends_on: [...],
  --   health: {url, expect_code, timeout_sec}, action: {build_cmd, start_cmd}}]
  status VARCHAR(32) NOT NULL DEFAULT 'idle',
  last_run_at DATETIME,
  last_result JSON,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
