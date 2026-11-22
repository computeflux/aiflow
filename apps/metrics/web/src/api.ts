// API 客户端: 默认同源(sslmon 进程同时托管 API 与静态页面)。
// 也可通过 VITE_SSL_API_URL 指向独立部署的 API(如 http://host:19003)。
export function getApiBase(): string {
  const raw = import.meta.env.VITE_SSL_API_URL as string | undefined;
  if (raw) return raw.trim().replace(/\/$/, "");
  return `${window.location.protocol}//${window.location.host}`;
}

export type CertStatus = "OK" | "WARN" | "EXPIRED" | "ERROR";

// 单个主机 TLS 证书检查结果(JSON 字段与 sslmon -json 输出一致)。
export interface CertReport {
  host: string;
  status: CertStatus;
  days_left: number;
  subject?: string;
  issuer?: string;
  sans?: string[];
  not_before?: string; // RFC3339 (UTC)
  not_after?: string; // RFC3339 (UTC)
  chain_error?: string;
  error?: string;
}

export type ServiceStatus = "UP" | "DOWN";

// 单个项目服务的健康检查结果(收到任意 HTTP 响应即 UP, 连接失败/超时 DOWN)。
export interface ServiceReport {
  name: string;
  url: string;
  status: ServiceStatus;
  latency_ms?: number;
  http_code?: number;
  error?: string;
}

// 单个主机的证书续签状态。
export interface RenewalInfo {
  host: string;
  days_left: number;
  due: boolean; // 达到自动续签阈值
  cert_ready: boolean; // 已有新证书可下载
  cert_not_after?: string;
  last_try?: string;
  last_error?: string;
}

export interface StatusData {
  collected_at: string; // RFC3339 (UTC)
  hosts: CertReport[];
  services?: ServiceReport[];
  renewals?: RenewalInfo[];
  error?: string;
}

export async function fetchStatus(): Promise<StatusData> {
  const res = await fetch(`${getApiBase()}/api/status`);
  if (!res.ok) throw new Error(`fetch status failed: ${res.status}`);
  return res.json();
}

// ---------------- 部署控制 ----------------

export interface Deployable {
  name: string;
  script: string;
  actions: string[];
}

export interface DeployConfig {
  enabled: boolean;
  deployables: Deployable[];
}

export interface DeployResult {
  ok: boolean;
  exit_code: number;
  duration_ms: number;
  output: string;
}

export async function fetchDeployConfig(): Promise<DeployConfig> {
  const res = await fetch(`${getApiBase()}/api/deploy/config`);
  if (!res.ok) throw new Error(`fetch deploy config failed: ${res.status}`);
  return res.json();
}

export async function sendDeploy(
  name: string,
  action: string,
  token: string,
): Promise<DeployResult> {
  const res = await fetch(`${getApiBase()}/api/deploy`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Deploy-Token": token },
    body: JSON.stringify({ name, action }),
  });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || `deploy failed: ${res.status}`);
  return data;
}

// ---------------- 证书续签 (Let's Encrypt) ----------------

export async function triggerRenew(
  host: string,
  token: string,
): Promise<{ ok: boolean; not_after?: string }> {
  const res = await fetch(`${getApiBase()}/api/renew`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Deploy-Token": token },
    body: JSON.stringify({ host }),
  });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || `renew failed: ${res.status}`);
  return data;
}

export type CertFile = "cert" | "fullchain" | "privkey";

export function certDownloadURL(host: string, file: CertFile): string {
  return `${getApiBase()}/api/cert/${encodeURIComponent(host)}/${file}`;
}

// ---------------- 面板设置(数据库配置) ----------------

// 与后端 config 结构一致(所有数据存于 MySQL monitor_config 表); 此处只做类型提示, 编辑用原始 JSON。
export interface MonitorConfig {
  hosts: string[];
  port?: number;
  warn_days?: number;
  services?: { name: string; url: string }[];
  deployables?: { name: string; script: string; actions: string[] }[];
  renewal?: {
    enabled?: boolean;
    days?: number;
    email?: string;
    dns?: string;
    staging?: boolean;
  };
  interval?: string;
  alert_to?: string[];
  smtp?: Record<string, unknown>;
  [key: string]: unknown;
}

export interface ConfigData {
  config: MonitorConfig;
  updated_at: string; // RFC3339 (UTC)
}

export async function fetchConfig(token: string): Promise<ConfigData> {
  const res = await fetch(`${getApiBase()}/api/config`, {
    headers: { "X-Deploy-Token": token },
  });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || `fetch config failed: ${res.status}`);
  return data;
}

// ---------------- 域名管理(增删监控域名, 30s 内热加载) ----------------

export interface HostsResult {
  ok: boolean;
  host?: string; // 仅添加接口返回
  hosts: string[];
  updated_at: string;
}

export async function addHost(host: string, token: string): Promise<HostsResult> {
  const res = await fetch(`${getApiBase()}/api/hosts`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Deploy-Token": token },
    body: JSON.stringify({ host }),
  });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || `add host failed: ${res.status}`);
  return data;
}

export async function removeHost(host: string, token: string): Promise<HostsResult> {
  const res = await fetch(`${getApiBase()}/api/hosts/${encodeURIComponent(host)}`, {
    method: "DELETE",
    headers: { "X-Deploy-Token": token },
  });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || `remove host failed: ${res.status}`);
  return data;
}

export async function saveConfig(
  cfg: MonitorConfig,
  token: string,
): Promise<{ ok: boolean; updated_at: string }> {
  const res = await fetch(`${getApiBase()}/api/config`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Deploy-Token": token },
    body: JSON.stringify(cfg),
  });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || `save config failed: ${res.status}`);
  return data;
}