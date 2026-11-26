export function getApiBase(): string {
  // 同源代理（nginx /api/ → workflow-api:8001，见 deploy/nginx/core-ui.conf），无跨域。
  // 必须返回绝对 URL：部分调用点用 new URL() 拼 query 参数，相对路径会抛 "not a valid URL"。
  return `${window.location.origin}/api`;
}

export function getMailerApiBase(): string {
  // 同源代理（nginx /mailer/ → mailer:8003，见 deploy/nginx/board.conf）；同上需绝对 URL
  return `${window.location.origin}/mailer`;
}

// 普通请求默认超时（快接口）。长任务不要用它：SSE/流式/拆解类请显式传更大的值或 authFetch（不设超时）。
const REQUEST_TIMEOUT_MS = 10_000;

// 拆解是长任务（本地大模型冷预填可达数分钟），超时必须大于 nginx（900s）与后端 DECOMPOSE_TIMEOUT_SEC（600s）：
// 不等式 后端 < nginx < 前端，出错时总是最内层先返回可读错误。
const DECOMPOSE_TIMEOUT_MS = 950_000;

// 统一认证（apps/auth）：请求自动带 Bearer；401 → 派发 auth:required
//（SsoGate 跳枢纽 /login 换新凭证）。
//
// ⚠️ 非枢纽服务禁止自行 POST /auth/refresh：refresh token 经 ?sso= 复制到各 origin
// 各持一份拷贝，谁先轮换谁的旧拷贝就是"重放"→ auth 的轮换+重放检测会吊销全部会话
//（auth_audit_logs: refresh_reuse_detected）。凭证轮换只由枢纽（board-ops /login）执行。
function getAuthToken(): string {
  try {
    const raw = localStorage.getItem("web3auth:session");
    if (!raw) return "";
    return (JSON.parse(raw) as { token?: string }).token ?? "";
  } catch {
    return "";
  }
}

// 401 弹登录节流：页面多接口并发 401 时只派发一次，避免事件风暴
let lastAuthRequiredAt = 0;
function emitAuthRequiredThrottled(): void {
  const now = Date.now();
  if (now - lastAuthRequiredAt < 5000) return;
  lastAuthRequiredAt = now;
  window.dispatchEvent(new Event("auth:required"));
}

// 凭证轮换注入点：只有枢纽 origin（core-ui）在 main.tsx 模块初始化时注册；
// 非枢纽服务不注册 → 401 先尝试 iframe 换新凭证（/ops/sso），失败才跳 /login。
let refreshHandler: (() => Promise<string | null>) | null = null;
export function setRefreshHandler(fn: (() => Promise<string | null>) | null): void {
  refreshHandler = fn;
}

// authFetch 带 Bearer 的 fetch，**不设超时中止**。专给 SSE / 长任务用：
// request() 会给每个请求挂 AbortSignal.timeout，流式响应（本地大模型慢）会被它提前中断。
// 401 → 有 token 时尝试一次静默续期并重放；仍 401 或本来就没 token → 派发 auth:required（去枢纽登录）。
export async function authFetch(input: string | URL, init?: RequestInit): Promise<Response> {
  const headers = new Headers(init?.headers);
  const tok = getAuthToken();
  if (tok && !headers.has("Authorization")) headers.set("Authorization", `Bearer ${tok}`);
  let res = await fetch(input, { ...init, headers });
  if (res.status === 401) {
    if (tok && refreshHandler) {
      const newTok = await refreshHandler();
      if (newTok) {
        headers.set("Authorization", `Bearer ${newTok}`);
        res = await fetch(input, { ...init, headers });
      }
    }
    if (res.status === 401) emitAuthRequiredThrottled();
  }
  return res;
}

export async function request(input: string | URL, init?: RequestInit, errorMsg?: string, timeoutMs = REQUEST_TIMEOUT_MS): Promise<Response> {
  const headers = new Headers(init?.headers);
  const tok = getAuthToken();
  if (tok && !headers.has("Authorization")) headers.set("Authorization", `Bearer ${tok}`);
  let res = await fetch(input, { ...init, headers, signal: AbortSignal.timeout(timeoutMs) });
  if (res.status === 401) {
    if (tok && refreshHandler) {
      // 枢纽：静默续期后重放一次（handler 内含全局单例锁，并发 401 只发一次 refresh）
      const newTok = await refreshHandler();
      if (newTok) {
        headers.set("Authorization", `Bearer ${newTok}`);
        res = await fetch(input, { ...init, headers, signal: AbortSignal.timeout(timeoutMs) });
      }
    }
    // 无 token / 续期失败 / 非枢纽 → 去枢纽获取凭证（不能就地续期，见上）
    if (res.status === 401) emitAuthRequiredThrottled();
  }
  if (!res.ok && errorMsg) throw new Error(`${errorMsg}: ${res.status}`);
  return res;
}

export function getAuthApiBase(): string {
  const raw = import.meta.env.VITE_AUTH_API_URL as string | undefined;
  if (raw) return raw.trim().replace(/\/$/, "");
  return `http://${window.location.hostname}:8008`;
}

// ---- 统一认证（apps/auth :8008）用户管理 ----
// 走 request：自动带 Bearer + 401 静默续期；仅 admin 角色可访问（auth 侧 RequireAdmin）。

export interface AuthUser {
  id: string;
  email?: string;
  name?: string;
  wallet_addr?: string;
  status: string; // active / disabled
  roles: string[];
  created_at: string;
}
export interface AuthUserListResponse { total: number; page: number; size: number; items: AuthUser[] }
export interface AuthRole { id: string; name: string; is_builtin: boolean; permissions: string[] }

async function authRequest(path: string, init?: RequestInit): Promise<any> {
  const res = await request(`${getAuthApiBase()}${path}`, init, `auth ${path} failed`);
  return res.json();
}
export function listAuthUsers(q = ""): Promise<AuthUserListResponse> {
  const qs = q ? `&q=${encodeURIComponent(q)}` : "";
  return authRequest(`/users?size=100${qs}`);
}
export function listAuthRoles(): Promise<{ roles: AuthRole[] }> {
  return authRequest("/roles");
}
export function createAuthUser(wallet: string, roles: string[]): Promise<{ ok: boolean; id: string; roles: string[] }> {
  return authRequest("/users", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ wallet, roles }),
  });
}
export function deleteAuthUser(id: string): Promise<{ ok: boolean }> {
  return authRequest(`/users/${id}`, { method: "DELETE" });
}

// 通用 GET（走 request：自动带 Bearer + 401 静默续期）。供页面本地 fetch 复用。
export async function apiGet(path: string): Promise<any> {
  const res = await request(`${getApiBase()}${path}`, undefined, `fetch ${path} failed`);
  return res.json();
}

export interface PublishJob { id: number; platform: string; account_id: string; content: string; status: string; created_at: string; approved_at?: string; published_at?: string; error_msg?: string; }
export interface RawItem { id: number; source: string; external_id: string; keyword: string; url: string; content: string; published_at?: string; fetched_at: string; }
export interface VectorPoint { id: string; payload: { content?: string; source?: string; url?: string; [key: string]: any } }
export interface SearchResultItem { id: string; score: number; payload: { content?: string; source?: string; url?: string; keyword?: string; [key: string]: any } }
export interface SearchResponse { documents: SearchResultItem[]; answer: string; sufficient: boolean; reasoning: string }
export interface SystemStatus { database: { total_raw_items: number; total_jobs: number; total_accounts: number; jobs_stat: { pending: number; approved: number; published: number; failed: number } }; active_tasks: Array<{ workflow_id: string; run_id: string; type: string; start_time: string; status: string }> }
export interface WorkflowExecution { workflow_id: string; run_id: string; type: string; status: string; start_time: string; close_time: string; history_length: number }
export interface WorkflowDetail { workflow_id: string; run_id: string; type: string; status: string; start_time: string; close_time: string; history_length: number; memo: Record<string, any>; search_attributes: Record<string, any> }
export interface WorkflowsResponse { workflows: WorkflowExecution[] }
export interface IngestTask { id: number; source: string; keyword: string; limit: number; mode: string; schedule: string; enabled: boolean; parallel_count: number; last_run_at?: string; last_status?: string; last_workflow_id?: string; created_at: string; updated_at: string }
export interface EmailBatch { id: number; org_name: string; status: string; total: number; sent: number; analysis: string; cc_list: string; custom_subject: string; custom_content: string; email_sender_id?: number; created_at: string; approved_at?: string; sent_at?: string }
export interface EmailRecipient { id: number; batch_id: number; email: string; name: string; repo: string; reason: string; selected: boolean; status: string }
export interface EmailTemplate { id: number; name: string; subject: string; body: string; content_type: string; is_default: boolean; created_at: string; updated_at: string }
export interface EmailSender {
  id: number;
  name: string;
  host: string;
  port: number;
  user: string;
  password?: string;
  from_address: string;
  from_name: string;
  daily_limit: number;
  used_today: number;
  enabled: boolean;
  is_default: boolean;
  created_at: string;
  updated_at: string;
}
export interface XAccount { id: number; machine_id: string; username: string; auth_token: string; ct0: string; enabled: boolean; daily_like_limit: number; cooldown_minutes: number; used_today: number; last_used_at?: string; created_at: string; updated_at: string }
export interface FollowTarget { id: number; machine_id: string; handle: string; enabled: boolean; scan_interval_minutes: number; max_like_per_scan: number; last_checked_at?: string; created_at: string; updated_at: string }


// ---- M1-4 账号管理页（social :8010 渠道账号池，经 core-ui nginx /social/ 同源代理）----
// social 不是 workflow-api：账号 CRUD / 状态机 / 挑号 / 触发登录都在 social 自己的
// REST 上（见 apps/social/internal/infra/infra.go）。登录态明文（session_ref）默认绝不
// 从 API 出（social 侧 include_session 需服务端显式开 SOCIAL_ALLOW_SESSION_EXPORT=1，
// 前端永不开启）；新增账号提交的 session_ref 由 social 用 SOCIAL_SESSION_KEY 加密入库。
export interface SocialAccountView {
  id: number;
  channel: string;
  account_id: string;
  machine_id: string;
  owner: string;
  login_name: string;
  cookies_path: string;
  status: string; // active | need_login | banned | cooldown
  cooldown_until?: string;
  last_used_at?: string;
  used_today: number;
  daily_limit: number;
  last_error: string;
  last_login_at?: string;
  last_login_error: string;
}

export function getSocialApiBase(): string {
  // 同源代理（nginx /social/ → social:8010，见 deploy/nginx/core-ui.conf），无跨域。
  return `${window.location.origin}/social`;
}

async function socialRequest(path: string, init?: RequestInit, errorMsg?: string): Promise<any> {
  const res = await request(`${getSocialApiBase()}${path}`, init, errorMsg ?? `social ${path} failed`);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error((data as any)?.error || `social ${path} failed: ${res.status}`);
  return data;
}

export async function fetchSocialAccounts(filter: { channel?: string; machineId?: string; status?: string } = {}): Promise<SocialAccountView[]> {
  const qs = new URLSearchParams();
  if (filter.channel) qs.set("channel", filter.channel);
  if (filter.machineId) qs.set("machine_id", filter.machineId);
  if (filter.status) qs.set("status", filter.status);
  const q = qs.toString();
  const data = await socialRequest(`/api/social/accounts${q ? `?${q}` : ""}`, undefined, "fetch social accounts failed");
  return (data?.accounts ?? []) as SocialAccountView[];
}

export async function createSocialAccount(data: { channel: string; account_id: string; machine_id?: string; owner?: string; login_name?: string; session_ref?: string; cookies_path?: string; status?: string; daily_limit?: number }): Promise<SocialAccountView> {
  const res = await socialRequest("/api/social/accounts", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "create social account failed");
  return res.account as SocialAccountView;
}

export async function deleteSocialAccount(channel: string, accountId: string): Promise<void> {
  await socialRequest(`/api/social/accounts/${encodeURIComponent(channel)}/${encodeURIComponent(accountId)}`, { method: "DELETE" }, "delete social account failed");
}

export async function setSocialAccountStatus(channel: string, accountId: string, status: string, cooldownMinutes = 0, lastError = ""): Promise<SocialAccountView> {
  const res = await socialRequest(`/api/social/accounts/${encodeURIComponent(channel)}/${encodeURIComponent(accountId)}/status`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ status, cooldown_minutes: cooldownMinutes, last_error: lastError }) }, "set social account status failed");
  return res.account as SocialAccountView;
}

// 触发登录：202 建 login 任务（Redis 就绪时入队，由 worker 执行登录并回报账号池）。
export async function triggerSocialAccountLogin(channel: string, accountId: string): Promise<{ job_no: string; queue: boolean }> {
  const res = await socialRequest(`/api/social/accounts/${encodeURIComponent(channel)}/${encodeURIComponent(accountId)}/login`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }, "trigger social account login failed");
  return { job_no: res.job_no as string, queue: !!res.queue };
}

export async function fetchSocialRegistryChannels(): Promise<string[]> {
  const data = await socialRequest("/api/social/registry", undefined, "fetch social registry failed");
  return ((data?.registry ?? []) as Array<{ name: string }>).map((r) => r.name);
}
export async function fetchJobs(status?: string): Promise<PublishJob[]> { const url = new URL(`${getApiBase()}/jobs`); if (status) url.searchParams.set("status", status); const res = await request(url.toString(), undefined, "fetch jobs failed"); const data = await res.json(); return data ?? []; }
export async function fetchRawItems(source?: string, page = 1, pageSize = 20): Promise<{ items: RawItem[]; total: number }> { const url = new URL(`${getApiBase()}/raw-items`); if (source) url.searchParams.set("source", source); url.searchParams.set("page", String(page)); url.searchParams.set("page_size", String(pageSize)); const res = await request(url.toString(), undefined, "fetch raw items failed"); const data = await res.json(); return { items: data?.items ?? [], total: data?.total ?? 0 }; }
export async function fetchVectorPoints(): Promise<VectorPoint[]> { const res = await request(`${getApiBase()}/vector-points`, undefined, "fetch vector points failed"); const data = await res.json(); return data ?? []; }
export async function searchVectorDB(query: string, k = 5): Promise<SearchResponse> { const url = new URL(`${getApiBase()}/search`); url.searchParams.set("q", query); url.searchParams.set("k", String(k)); const res = await request(url.toString(), undefined, "search failed"); const data = await res.json(); const docs: SearchResultItem[] = (data.documents || []).map((d: any) => ({ id: d.chunk?.id || "", score: d.score || 0, payload: { content: d.chunk?.content || "", source: d.chunk?.source || "", url: d.chunk?.url || "", keyword: d.chunk?.keyword || "" } })); return { documents: docs, answer: data.answer || "", sufficient: data.sufficient || false, reasoning: data.reasoning || "" }; }
export async function fetchStatus(): Promise<SystemStatus> { const res = await request(`${getApiBase()}/status`, undefined, "fetch status failed"); const data = await res.json() ?? {}; const db = data.database ?? {}; return { database: { total_raw_items: db.total_raw_items ?? 0, total_jobs: db.total_jobs ?? 0, total_accounts: db.total_accounts ?? 0, jobs_stat: db.jobs_stat ?? { pending: 0, approved: 0, published: 0, failed: 0 } }, active_tasks: data.active_tasks ?? [] }; }
export async function fetchWorkflows(status = ""): Promise<WorkflowsResponse> { const url = new URL(`${getApiBase()}/workflows`); if (status) url.searchParams.set("status", status); const res = await request(url.toString(), undefined, "fetch workflows failed"); const data = await res.json(); return { workflows: data?.workflows ?? [] }; }
export async function fetchWorkflowDetail(workflowId: string, runId: string): Promise<WorkflowDetail> { const res = await request(`${getApiBase()}/workflows/${workflowId}/${runId}`, undefined, "fetch workflow detail failed"); return res.json(); }
export async function cancelWorkflow(workflowId: string, runId?: string): Promise<void> { const url = new URL(`${getApiBase()}/workflows/${workflowId}/cancel`); if (runId) url.searchParams.set("run_id", runId); await request(url.toString(), { method: "POST" }, "cancel workflow failed"); }
export async function terminateWorkflow(workflowId: string, runId?: string): Promise<void> { const url = new URL(`${getApiBase()}/workflows/${workflowId}/terminate`); if (runId) url.searchParams.set("run_id", runId); await request(url.toString(), { method: "POST" }, "terminate workflow failed"); }
export async function fetchIngestTasks(page = 1, pageSize = 50): Promise<{ tasks: IngestTask[]; total: number }> { const res = await request(`${getApiBase()}/ingest-tasks?page=${page}&page_size=${pageSize}`, undefined, "fetch ingest tasks failed"); const data = await res.json(); return { tasks: data?.tasks ?? [], total: data?.total ?? 0 }; }
export async function createIngestTask(data: { source: string; keyword: string; limit?: number; mode?: string; schedule?: string; enabled?: boolean; parallel_count?: number }): Promise<{ task: IngestTask }> { const res = await request(`${getApiBase()}/ingest-tasks`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "create ingest task failed"); return res.json(); }
export async function updateIngestTask(id: number, data: any): Promise<{ task: IngestTask }> { const res = await request(`${getApiBase()}/ingest-tasks/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "update ingest task failed"); return res.json(); }
export async function deleteIngestTask(id: number): Promise<void> { await request(`${getApiBase()}/ingest-tasks/${id}`, { method: "DELETE" }, "delete ingest task failed"); }
export async function runIngestTask(id: number): Promise<{ workflow_id: string; run_id: string }> { const res = await request(`${getApiBase()}/ingest-tasks/${id}/run`, { method: "POST" }, "run ingest task failed"); return res.json(); }
export async function spreadIngestTasks(source: string): Promise<{ message: string; count: number }> { const res = await request(`${getApiBase()}/ingest-tasks/spread`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ source }) }, "spread failed"); return res.json(); }
export async function restartCrons(): Promise<{ total: number; success: number; failed: number }> { const res = await request(`${getApiBase()}/ingest-tasks/restart-crons`, { method: "POST" }, "restart crons failed"); return res.json(); }
export async function approveJob(id: number): Promise<void> { await request(`${getApiBase()}/jobs/${id}/approve`, { method: "POST" }, "approve failed"); }
export async function rejectJob(id: number, reason?: string): Promise<void> { await request(`${getApiBase()}/jobs/${id}/reject`, { method: "POST", headers: { "Content-Type": "application/json" }, body: reason ? JSON.stringify({ reason }) : undefined }, "reject failed"); }
export async function publishJob(jobId: number): Promise<void> { await request(`${getApiBase()}/publish`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ job_id: jobId }) }, "publish failed"); }
export async function startEmailWorkflow(orgName: string): Promise<{ workflow_id: string; run_id: string }> { const res = await request(`${getMailerApiBase()}/email`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ org_name: orgName }) }, "start email workflow failed"); return res.json(); }
export async function fetchGitHubOrgs(): Promise<string[]> { const res = await request(`${getMailerApiBase()}/email/github-orgs`, undefined, "fetch github orgs failed"); const data = await res.json(); return data?.orgs ?? []; }
export async function fetchEmailBatches(): Promise<{ batches: EmailBatch[] }> { const res = await request(`${getMailerApiBase()}/email-batches`, undefined, "fetch email batches failed"); const data = await res.json(); return { batches: data?.batches ?? [] }; }
export async function fetchEmailBatch(id: number): Promise<{ batch: EmailBatch; recipients: EmailRecipient[] }> { const res = await request(`${getMailerApiBase()}/email-batches/${id}`, undefined, "fetch email batch failed"); const data = await res.json() ?? {}; return { batch: data.batch ?? {}, recipients: data.recipients ?? [] }; }
export async function approveEmailBatch(id: number): Promise<void> { await request(`${getMailerApiBase()}/email-batches/${id}/approve`, { method: "POST" }, "approve email batch failed"); }
export async function sendEmailBatch(id: number, data?: { content?: string; subject?: string; cc_list?: string; recipient_ids?: number[]; email_sender_id?: number }): Promise<void> { await request(`${getMailerApiBase()}/email-batches/${id}/send`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data || {}) }, "send email batch failed"); }
export async function rejectEmailBatch(id: number): Promise<void> { await request(`${getMailerApiBase()}/email-batches/${id}/reject`, { method: "POST" }, "reject email batch failed"); }
export async function deleteEmailBatch(id: number): Promise<void> { await request(`${getMailerApiBase()}/email-batches/${id}`, { method: "DELETE" }, "delete email batch failed"); }
export interface EmailRecipientStat { id: number; email: string; name: string; repo: string; reason: string; selected: boolean; status: string; count: number }
export async function fetchEmailSendStats(id: number): Promise<{ recipients: EmailRecipientStat[] }> { const res = await request(`${getMailerApiBase()}/email-batches/${id}/send-stats`, undefined, "fetch send stats failed"); const data = await res.json(); return { recipients: data?.recipients ?? [] }; }
export async function fetchEmailTemplates(): Promise<{ templates: EmailTemplate[] }> { const res = await request(`${getMailerApiBase()}/email-templates`, undefined, "fetch email templates failed"); const data = await res.json(); return { templates: data?.templates ?? [] }; }
export async function createEmailTemplate(data: { name: string; subject: string; body: string; content_type?: string; is_default?: boolean }): Promise<{ template: EmailTemplate }> { const res = await request(`${getMailerApiBase()}/email-templates`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "create email template failed"); return res.json(); }
export async function updateEmailTemplate(id: number, data: { name?: string; subject?: string; body?: string; content_type?: string; is_default?: boolean }): Promise<{ template: EmailTemplate }> { const res = await request(`${getMailerApiBase()}/email-templates/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "update email template failed"); return res.json(); }
export async function deleteEmailTemplate(id: number): Promise<void> { await request(`${getMailerApiBase()}/email-templates/${id}`, { method: "DELETE" }, "delete email template failed"); }
export async function fetchEmailSenders(): Promise<{ senders: EmailSender[] }> { const res = await request(`${getMailerApiBase()}/email-senders`, undefined, "fetch email senders failed"); const data = await res.json(); return { senders: data?.senders ?? [] }; }
export async function createEmailSender(data: { name: string; host: string; port?: number; user: string; password?: string; from_address: string; from_name?: string; daily_limit?: number; enabled?: boolean; is_default?: boolean }): Promise<EmailSender> { const res = await request(`${getMailerApiBase()}/email-senders`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "create email sender failed"); return res.json(); }
export async function updateEmailSender(id: number, data: Partial<{ name: string; host: string; port: number; user: string; password: string; from_address: string; from_name: string; daily_limit: number; enabled: boolean; is_default: boolean }>): Promise<EmailSender> { const res = await request(`${getMailerApiBase()}/email-senders/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "update email sender failed"); return res.json(); }
export async function deleteEmailSender(id: number): Promise<void> { await request(`${getMailerApiBase()}/email-senders/${id}`, { method: "DELETE" }, "delete email sender failed"); }
export async function testEmailSender(id: number, to?: string): Promise<{ message: string }> { const res = await request(`${getMailerApiBase()}/email-senders/${id}/test`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ to: to || "" }) }, "test email sender failed"); return res.json(); }
export async function fetchXAccounts(machineId = ""): Promise<XAccount[]> { const url = new URL(`${getApiBase()}/x-accounts`); if (machineId) url.searchParams.set("machine_id", machineId); const res = await request(url.toString(), undefined, "fetch x accounts failed"); const data = await res.json(); return data?.accounts ?? []; }
export async function createXAccount(data: { machine_id?: string; username: string; auth_token: string; ct0: string; enabled?: boolean; daily_like_limit?: number; cooldown_minutes?: number }): Promise<XAccount> { const res = await request(`${getApiBase()}/x-accounts`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }); if (!res.ok) { const e = await res.json().catch(() => ({})); throw new Error(e.error || `create x account failed: ${res.status}`); } return res.json(); }
export async function updateXAccount(id: number, data: Partial<{ machine_id: string; username: string; auth_token: string; ct0: string; enabled: boolean; daily_like_limit: number; cooldown_minutes: number }>): Promise<XAccount> { const res = await request(`${getApiBase()}/x-accounts/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "update x account failed"); return res.json(); }
export async function deleteXAccount(id: number): Promise<void> { await request(`${getApiBase()}/x-accounts/${id}`, { method: "DELETE" }, "delete x account failed"); }
export async function fetchFollowTargets(machineId = ""): Promise<FollowTarget[]> { const url = new URL(`${getApiBase()}/follow-targets`); if (machineId) url.searchParams.set("machine_id", machineId); const res = await request(url.toString(), undefined, "fetch follow targets failed"); const data = await res.json(); return data?.targets ?? []; }
export async function createFollowTarget(data: { machine_id?: string; handle: string; enabled?: boolean; scan_interval_minutes?: number; max_like_per_scan?: number }): Promise<FollowTarget> { const res = await request(`${getApiBase()}/follow-targets`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }); if (!res.ok) { const e = await res.json().catch(() => ({})); throw new Error(e.error || `create follow target failed: ${res.status}`); } return res.json(); }
export async function updateFollowTarget(id: number, data: Partial<{ machine_id: string; handle: string; enabled: boolean; scan_interval_minutes: number; max_like_per_scan: number }>): Promise<FollowTarget> { const res = await request(`${getApiBase()}/follow-targets/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "update follow target failed"); return res.json(); }
export async function deleteFollowTarget(id: number): Promise<void> { await request(`${getApiBase()}/follow-targets/${id}`, { method: "DELETE" }, "delete follow target failed"); }
export async function dispatchFollowTarget(id: number): Promise<{ workflow_id: string; run_id: string }> { const res = await request(`${getApiBase()}/follow-targets/${id}/dispatch`, { method: "POST" }); if (!res.ok) { const e = await res.json().catch(() => ({})); throw new Error(e.error || `dispatch follow target failed: ${res.status}`); } return res.json(); }
export interface SocialTweet { score: number; source_url: string; keyword: string; tweet: string; reasons: string[] }
export interface SocialGenerateResult { tweets: SocialTweet[]; total_scored: number }
export async function generateSocialContent(hours: number, count: number, source: string): Promise<SocialGenerateResult> { const res = await request(`${getApiBase()}/social-generate`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ hours, count, source }) }, "social generate failed"); return res.json(); }
export interface Board { id: number; name: string; description: string; view_mode: string; created_at: string; updated_at: string; lists: BoardList[] }
export interface BoardList { id: number; board_id: number; name: string; position: number; cards: BoardCard[] }
export interface CardDependency { card_id: number; title: string; status: string; merged: boolean }
export interface BoardCard { id: number; list_id: number; title: string; description: string; position: number; assignee: string; labels: string; target_name?: string; due_date?: string; priority: string; cover_color: string; checklist: string; source: string; workflow_id: string; status?: string; work_item_no?: string; source_ref?: string; frequency?: number; depends_on?: string; blocked_by?: CardDependency[]; evolve_status?: string; fail_reason?: string; merge_state?: string; created_at: string; updated_at: string; comments?: BoardComment[] }
export interface BoardComment { id: number; card_id: number; author: string; body: string; created_at: string }
export async function fetchBoards(): Promise<{ boards: Board[] }> { const res = await request(`${getApiBase()}/boards`, undefined, "fetch boards failed"); const data = await res.json(); return { boards: data?.boards ?? [] }; }
export async function createBoard(name: string, viewMode: string): Promise<{ board: Board }> { const res = await request(`${getApiBase()}/boards`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name, view_mode: viewMode }) }, "create board failed"); return res.json(); }
export async function fetchBoard(id: number): Promise<Board> { const res = await request(`${getApiBase()}/boards/${id}`, undefined, "fetch board failed"); return res.json(); }
export async function deleteBoard(id: number): Promise<void> { await request(`${getApiBase()}/boards/${id}`, { method: "DELETE" }, "delete board failed"); }
export async function renameBoard(id: number, name: string): Promise<void> { await request(`${getApiBase()}/boards/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name }) }, "rename board failed"); }
export async function createCard(listId: number, title: string, extra?: Record<string, any>): Promise<BoardCard> { const res = await request(`${getApiBase()}/cards`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ list_id: listId, title, ...extra }) }, "create card failed"); return res.json(); }
export async function deleteList(id: number): Promise<void> { await request(`${getApiBase()}/lists/${id}`, { method: "DELETE" }, "delete list failed"); }
// updateCard：timeoutMs 可调（拖到 In Progress 会触发 LLM 依赖检查，需要更长超时）。
// 409（前置未完成）时把后端 error/blocked_by 拼成可读信息抛给 UI。
export async function updateCard(id: number, data: Record<string, any>, timeoutMs = 10_000): Promise<void> {
  const res = await request(`${getApiBase()}/cards/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, undefined, timeoutMs);
  if (!res.ok) {
    const body = await res.json().catch(() => null);
    const list = Array.isArray(body?.blocked_by) ? body.blocked_by : [];
    const blockers = list.length
      ? `（需先完成：${list.map((b: any) => `#${b.card_id} ${b.title}`).join("、")}）`
      : "";
    throw new Error((body?.error || `update card failed: ${res.status}`) + blockers);
  }
}
// 人工结束在跑任务（In Progress 卡不能直接跳到 shipped/open —— 状态机禁止跳级）：
// outcome=done → 停任务 + 逐级到 shipped（卡 → Done）；reject → 停任务 + rejected（卡 → Backlog）
export async function finishCard(cardId: number, outcome: "done" | "reject" | "review", reason = ""): Promise<{ work_item: string; status: string; path: string[] }> {
  const res = await request(`${getApiBase()}/cards/${cardId}/finish`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ outcome, reason }) }, undefined, 30_000);
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error || `结束任务失败（HTTP ${res.status}）`);
  return data;
}
// skipCardReview：跳过 AI 审核（reviewer/ocr），直接继续沙箱/CI → 进 Review 列由人工看 diff。
// 用于 diff 过大（一次 vendor 上万行）或 reviewer 反复超时的任务——
// 逐行自动审只会耗满轮次上限再 cancel。标记下一轮生效。
export async function skipCardReview(cardId: number): Promise<{ ok: boolean; task_no: string; task_status: string; note: string }> {
  const res = await request(`${getApiBase()}/cards/${cardId}/skip-review`, { method: "POST" }, undefined, 30_000);
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error || `跳过审核失败（HTTP ${res.status}）`);
  return data;
}
export async function deleteCard(id: number): Promise<void> { await request(`${getApiBase()}/cards/${id}`, { method: "DELETE" }, "delete card failed"); }
export async function fetchCardComments(cardId: number): Promise<{ comments: BoardComment[] }> { const res = await request(`${getApiBase()}/cards/${cardId}/comments`, undefined, "fetch comments failed"); return res.json(); }
export async function addCardComment(cardId: number, author: string, body: string): Promise<BoardComment> { const res = await request(`${getApiBase()}/cards/${cardId}/comments`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ author, body }) }, "add comment failed"); return res.json(); }
export async function createList(boardId: number, name: string): Promise<BoardList> { const res = await request(`${getApiBase()}/boards/${boardId}/lists`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name }) }, "create list failed"); return res.json(); }
export interface MindMapData { id: number; name: string; data: string; layout: string; theme: string; view_x: number; view_y: number; scale: number; created_at: string; updated_at: string }
export async function fetchMindMaps(): Promise<{ mindmaps: MindMapData[] }> { const res = await request(`${getApiBase()}/mindmaps`, undefined, "fetch mindmaps failed"); const data = await res.json(); return { mindmaps: data?.mindmaps ?? [] }; }
export async function createMindMap(data: { name: string; data: string; layout: string; theme: string; view_x?: number; view_y?: number; scale?: number }): Promise<{ mindmap: MindMapData }> { const res = await request(`${getApiBase()}/mindmaps`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "create mindmap failed"); return res.json(); }
export async function updateMindMap(id: number, data: Record<string, any>): Promise<void> { await request(`${getApiBase()}/mindmaps/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "update mindmap failed"); }
export async function deleteMindMap(id: number): Promise<void> { await request(`${getApiBase()}/mindmaps/${id}`, { method: "DELETE" }, "delete mindmap failed"); }
export async function mindMapChat(mindmapJson: string, message: string): Promise<{ reply: string; mindmap_json: string }> { const res = await request(`${getApiBase()}/mindmap-chat`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ mindmap_json: mindmapJson, message }) }, "mindmap chat failed"); return res.json(); }
export interface ErrorLog { id: number; system: string; level: string; source: string; workflow_id: string; run_id: string; message: string; stack: string; context: string; trace_id: string; work_item_no?: string; resolved: boolean; created_at: string; resolved_at?: string }
export interface ErrorStats { total: number; today: number; unresolved: number; by_system: { key: string; count: number }[]; by_level: { key: string; count: number }[] }
export async function fetchErrorLogs(params: { system?: string; level?: string; resolved?: string; q?: string; page?: number; page_size?: number } = {}): Promise<{ items: ErrorLog[]; total: number; page: number; page_size: number }> { const url = new URL(`${getApiBase()}/errors`); if (params.system) url.searchParams.set("system", params.system); if (params.level) url.searchParams.set("level", params.level); if (params.resolved) url.searchParams.set("resolved", params.resolved); if (params.q) url.searchParams.set("q", params.q); url.searchParams.set("page", String(params.page || 1)); url.searchParams.set("page_size", String(params.page_size || 20)); const res = await request(url.toString(), undefined, "fetch error logs failed"); const data = await res.json(); return { items: data?.items ?? [], total: data?.total ?? 0, page: data?.page ?? 1, page_size: data?.page_size ?? 20 }; }
export async function fetchErrorStats(): Promise<ErrorStats> { const res = await request(`${getApiBase()}/errors/stats`, undefined, "fetch error stats failed"); const data = await res.json(); return { total: data?.total ?? 0, today: data?.today ?? 0, unresolved: data?.unresolved ?? 0, by_system: data?.by_system ?? [], by_level: data?.by_level ?? [] }; }
export async function resolveErrorLog(id: number): Promise<void> { await request(`${getApiBase()}/errors/${id}/resolve`, { method: "POST" }, "resolve error log failed"); }
export async function resolveAllErrors(): Promise<{ updated: number }> { const res = await request(`${getApiBase()}/errors/resolve-all`, { method: "POST" }, "resolve all errors failed"); return res.json(); }
export async function deleteErrorLog(id: number): Promise<void> { await request(`${getApiBase()}/errors/${id}`, { method: "DELETE" }, "delete error log failed"); }

// ---- Skill 框架（P1，docs/plan/transform-plan.md §6.9）----
export interface SkillVersionInfo { id: number; version: string; image: string; traffic_split: number; status: string; rollback_to: string; config: Record<string, any> | null; created_at: string }
export interface SkillRunRecord { id: number; skill_name: string; version: string; trace_id: string; workflow_id: string; run_id: string; input_hash: string; status: string; latency_ms: number | null; error: string; created_at: string }
export interface SkillInfo { id: number; name: string; owner: string; runtime: string; type: string; entrypoint: string; addr: string; container: string; limits: Record<string, any> | null; permissions: Record<string, any> | null; input_schema: Record<string, any> | null; output_schema: Record<string, any> | null; live_version: string; versions: SkillVersionInfo[]; recent_runs: SkillRunRecord[]; run_count: number; created_at: string; updated_at: string }
export async function fetchSkills(): Promise<SkillInfo[]> { const res = await request(`${getApiBase()}/api/skills`, undefined, "fetch skills failed"); const data = await res.json(); return data?.skills ?? []; }
export async function fetchSkill(name: string): Promise<SkillInfo> { const res = await request(`${getApiBase()}/api/skills/${name}`, undefined, "fetch skill failed"); return res.json(); }
export async function fetchSkillRuns(name: string, limit = 50): Promise<SkillRunRecord[]> { const res = await request(`${getApiBase()}/api/skills/${name}/runs?limit=${limit}`, undefined, "fetch skill runs failed"); const data = await res.json(); return data?.runs ?? []; }
export async function invokeSkill(name: string, input: Record<string, any>): Promise<Record<string, any>> { const res = await request(`${getApiBase()}/api/skills/${name}/invoke`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(input) }, undefined, 310_000); if (!res.ok) { const e = await res.json().catch(() => ({})); const err: any = new Error(e.error || `invoke skill failed: ${res.status}`); err.output = e.output; err.status = res.status; throw err; } return res.json(); }
export async function registerSkill(specJson: string): Promise<{ skill: SkillInfo; version: SkillVersionInfo; created: boolean }> { const res = await request(`${getApiBase()}/api/skills`, { method: "POST", headers: { "Content-Type": "application/json" }, body: specJson }, undefined); if (!res.ok) { const e = await res.json().catch(() => ({})); throw new Error(e.error || `register skill failed: ${res.status}`); } return res.json(); }
export async function setSkillTraffic(name: string, version: string, trafficSplit: number): Promise<SkillVersionInfo> { const res = await request(`${getApiBase()}/api/skills/${name}/versions/${version}/traffic`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ traffic_split: trafficSplit }) }, undefined); if (!res.ok) { const e = await res.json().catch(() => ({})); throw new Error(e.error || `set skill traffic failed: ${res.status}`); } return res.json(); }

// ---- S1 进化闭环：审批 / Agent / 进化单（docs/plan/S1-进化闭环.md §3.9）----
export interface Approval { id: number; approval_no: string; kind: string; subject: string; payload: any; status: "pending" | "approved" | "rejected" | "expired"; proposer: string; approver: string | null; reason: string | null; expires_at: string | null; created_at: string; decided_at: string | null }
export async function listApprovals(status = "", limit = 50): Promise<Approval[]> { const url = new URL(`${getApiBase()}/approvals`); if (status) url.searchParams.set("status", status); url.searchParams.set("limit", String(limit)); const res = await request(url.toString(), undefined, "fetch approvals failed"); const data = await res.json(); return data?.approvals ?? []; }
export async function getApproval(id: number): Promise<Approval> { const res = await request(`${getApiBase()}/approvals/${id}`, undefined, "fetch approval failed"); return res.json().then((d: any) => d.approval); }
export async function decideApproval(id: number, decision: "approve" | "reject", approver: string, reason: string): Promise<Approval> { const res = await request(`${getApiBase()}/approvals/${id}/${decision}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ approver, reason }) }, "approval decision failed", 30_000); if (!res.ok) { const e = await res.json().catch(() => ({})); throw new Error(e.error || `approval ${decision} failed: ${res.status}`); } return res.json().then((d: any) => d.approval); }
export interface AgentInfo { name: string; role: "coder" | "reviewer"; version: string; entrypoint: string; container: string; workdir_prefix: string; model_profile?: string }
export interface AgentBindingInfo { agent_name: string; version: string; updated_by: string | null; updated_at: string | null }
export async function listAgents(): Promise<{ agents: AgentInfo[]; bindings: Record<string, AgentBindingInfo> }> { const res = await request(`${getApiBase()}/agents`, undefined, "fetch agents failed"); const data = await res.json(); return { agents: data?.agents ?? [], bindings: data?.bindings ?? {} }; }
export async function registerAgent(m: Partial<AgentInfo>): Promise<AgentInfo> { const res = await request(`${getApiBase()}/agents`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(m) }, "register agent failed"); return res.json().then((d: any) => d.agent); }
export async function bindAgent(role: "coder" | "reviewer", agentName: string, updatedBy = "web"): Promise<{ role: string; agent: string }> { const res = await request(`${getApiBase()}/agents/${role}/bind`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ agent_name: agentName, updated_by: updatedBy }) }, "bind agent failed"); return res.json(); }
// S8.2 agent 运行时模型：把模型档案绑到指定 agent（modelProfile 空 = 清除，回退全局/env）。
export async function setAgentModel(agentName: string, modelProfile: string): Promise<{ ok: boolean; agent: string; model_profile: string }> { const res = await request(`${getApiBase()}/agents/${encodeURIComponent(agentName)}/model`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ model_profile: modelProfile }) }, "set agent model failed"); if (!res.ok) { const e = await res.json().catch(() => ({})); throw new Error(e.error || `set agent model failed: ${res.status}`); } return res.json(); }
export interface EvolveTaskSummary { id: number; task_no: string; source: string; target_name: string; workdir: string | null; status: string; approval_id: number | null; created_at: string; updated_at: string }
export interface EvolveTaskDetail extends EvolveTaskSummary { evidence: any; diff: string; review: any; test_report: any }
export async function listEvolveTasks(status = "", limit = 50): Promise<EvolveTaskSummary[]> { const url = new URL(`${getApiBase()}/evolve-tasks`); if (status) url.searchParams.set("status", status); url.searchParams.set("limit", String(limit)); const res = await request(url.toString(), undefined, "fetch evolve tasks failed"); const data = await res.json(); return data?.tasks ?? []; }
export async function getEvolveTask(id: number): Promise<EvolveTaskDetail> { const res = await request(`${getApiBase()}/evolve-tasks/${id}`, undefined, "fetch evolve task failed"); return res.json().then((d: any) => d.task); }

// ---- S8 模型管理（model_profiles）----
export interface ModelProfile {
  id: number; name: string; provider: string; model: string;
  base_url: string; has_api_key: boolean; embedding_model: string;
  consumers: string; sync_agents?: number; active: number; note: string;
  updated_by: string; updated_at: string;
}
export interface ModelProfileBody {
  name: string; provider?: string; model: string; base_url?: string;
  api_key?: string; embedding_model?: string; consumers?: string; note?: string;
  sync_agents?: boolean;
}
export async function listModelProfiles(): Promise<ModelProfile[]> { const res = await request(`${getApiBase()}/model-config`, undefined, "fetch models failed"); const data = await res.json(); return data?.models ?? []; }
export async function createModelProfile(b: ModelProfileBody): Promise<ModelProfile> { const res = await request(`${getApiBase()}/model-config`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(b) }, "create model failed"); const d = await res.json(); if (!res.ok) throw new Error((d as any)?.error || "create model failed"); return d.model; }
export async function updateModelProfile(id: number, b: ModelProfileBody): Promise<ModelProfile> { const res = await request(`${getApiBase()}/model-config/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(b) }, "update model failed"); const d = await res.json(); if (!res.ok) throw new Error((d as any)?.error || "update model failed"); return d.model; }
export async function deleteModelProfile(id: number): Promise<void> { const res = await request(`${getApiBase()}/model-config/${id}`, { method: "DELETE" }, "delete model failed"); if (!res.ok) { const e = await res.json().catch(() => ({})); throw new Error(e.error || `delete model failed: ${res.status}`); } }
export async function applyModelProfile(id: number): Promise<{ applied: ModelProfile; aiflow?: string; agents?: Record<string, string> }> { const res = await request(`${getApiBase()}/model-config/${id}/apply`, { method: "POST" }, "apply model failed", 130_000); const d = await res.json(); if (!res.ok) throw new Error((d as any)?.error || "apply model failed"); return d; }
export async function syncAIFlowModels(): Promise<{ aiflow: string }> { const res = await request(`${getApiBase()}/model-config/sync-aiflow`, { method: "POST" }, "sync aiflow failed", 130_000); const d = await res.json(); if (!res.ok) throw new Error((d as any)?.error || "sync aiflow failed"); return d; }

// ---- OCR review-web：审核记录查询（wi-1793188513-b804，后端 GET /ocr/reviews）----
// 只读接口（需登录）：审核记录列表（分页 / 状态筛选）+ 单条审核过程详情。
export interface OcrReviewListItem {
  task_no: string;
  target_name: string;
  status: string;
  source: string;
  comment_count: number;
  error_count: number;
  warning_count: number;
  info_count: number;
  created_at: string;
  updated_at: string;
}
export interface OcrReviewListResponse { items: OcrReviewListItem[]; total: number; page: number; page_size: number }
export interface OcrReviewListFilter { task_no?: string; target_name?: string; status?: string; source?: string; page?: number; page_size?: number }
export async function fetchOcrReviews(f: OcrReviewListFilter = {}): Promise<OcrReviewListResponse> {
  const url = new URL(`${getApiBase()}/ocr/reviews`);
  if (f.task_no) url.searchParams.set("task_no", f.task_no);
  if (f.target_name) url.searchParams.set("target_name", f.target_name);
  if (f.status) url.searchParams.set("status", f.status);
  if (f.source) url.searchParams.set("source", f.source);
  url.searchParams.set("page", String(f.page || 1));
  url.searchParams.set("page_size", String(f.page_size || 20));
  const res = await request(url.toString(), undefined, "fetch ocr reviews failed");
  const data = await res.json();
  return { items: data?.items ?? [], total: data?.total ?? 0, page: data?.page ?? 1, page_size: data?.page_size ?? 20 };
}

// 单条审核过程详情（GET /ocr/reviews/:task_no）：任务元数据 + 解析后的审核评论
// + 步骤时间线 + 审计事件流 + 原始产物（diff / review JSON）。
export interface OcrReviewStep {
  name: string;
  status: "pending" | "running" | "done";
  at?: string | null;
  detail?: any;
}
export interface OcrReviewEvent {
  action: string;
  actor: string;
  at: string;
  detail: string;
  note: string;
}
export interface OcrReviewComment {
  file: string;
  line: number;
  level: string; // error / warning / info
  message: string;
}
export interface OcrReviewParsed {
  agent: string;
  comments: OcrReviewComment[];
  summary: { total: number; error: number; warning: number; info: number };
  reviewer_retries: number;
}
export interface OcrReviewTask {
  id: number;
  task_no: string;
  source: string;
  target_name: string;
  workdir: string;
  status: string;
  diff: string;
  review: string;
  approval_id: number | null;
  pr_url: string;
  merge_commit: string;
  created_at: string;
  updated_at: string;
}
export interface OcrReviewDetail {
  task: OcrReviewTask;
  review: OcrReviewParsed | null;
  steps: OcrReviewStep[];
  events: OcrReviewEvent[];
  artifacts: { diff: string; review_json: string };
}
export async function fetchOcrReviewDetail(taskNo: string): Promise<OcrReviewDetail> {
  const res = await request(`${getApiBase()}/ocr/reviews/${encodeURIComponent(taskNo)}`, undefined, "fetch ocr review detail failed");
  const data = await res.json();
  if (!data?.review) throw new Error("fetch ocr review detail failed: 响应缺少 review 字段");
  return data.review as OcrReviewDetail;
}

// ---- S3 干预舱（docs/plan/S3-干预舱.md §3.4）----
export interface Intervention { id: number; workflow_id: string; run_id: string | null; kind: string; status: "open" | "resolved"; note: string | null; created_at: string; resolved_at: string | null }
export interface ActiveWorkflow { workflow_id: string; namespace: string; status: string; start_time: string; event_count: number; last_error: string; max_consecutive_activity_failures: number }
export interface CircuitState { skill_name: string; state: "close" | "open" | "half"; consecutive_failures: number; opened_at: string | null; updated_at: string }
export async function fetchInterventions(status = ""): Promise<Intervention[]> { const url = new URL(`${getApiBase()}/interventions`); if (status) url.searchParams.set("status", status); const res = await request(url.toString(), undefined, "fetch interventions failed"); const data = await res.json(); return data?.items ?? []; }
export async function fetchActiveWorkflows(): Promise<ActiveWorkflow[]> { const res = await request(`${getApiBase()}/interventions/active-workflows`, undefined, "fetch active workflows failed", 30_000); const data = await res.json(); return data?.items ?? []; }
export async function fetchCircuitStates(): Promise<CircuitState[]> { const res = await request(`${getApiBase()}/interventions/circuit`, undefined, "fetch circuit states failed"); const data = await res.json(); return data?.items ?? []; }
async function interventionAction(id: number, action: string, body: Record<string, any>, timeoutMs = 30_000): Promise<any> { const res = await request(`${getApiBase()}/interventions/${id}/${action}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }, `${action} failed`, timeoutMs); if (!res.ok) { const e = await res.json().catch(() => ({})); throw new Error(e.error || `${action} failed: ${res.status}`); } return res.json(); }
export function takeoverIntervention(id: number, body: { workflow: string; input: Record<string, any> }): Promise<any> { return interventionAction(id, "takeover", body); }
export function injectIntervention(id: number, body: { name: string; payload: Record<string, any> }): Promise<any> { return interventionAction(id, "inject", body); }
export function retryIntervention(id: number, body: { workflow: string; input: Record<string, any>; skill?: string }): Promise<any> { return interventionAction(id, "retry", body); }
export function rollbackIntervention(id: number, body: { skill_name: string; rollback_to: string }): Promise<any> { return interventionAction(id, "rollback", body); }
export function resolveIntervention(id: number, note = ""): Promise<any> { return interventionAction(id, "resolve", { note }, 15_000); }

// ---- S6 运维情报（docs/plan/S6-运维情报.md）----
export interface OpsReport { id: number; report_date: string; type: string; content: string; metrics: any; created_at: string }
export async function fetchOpsReports(type = ""): Promise<{ items: OpsReport[]; total: number }> { const url = new URL(`${getApiBase()}/ops-reports`); if (type) url.searchParams.set("type", type); const res = await request(url.toString(), undefined, "fetch ops reports failed"); const data = await res.json(); return { items: data?.items ?? [], total: data?.total ?? 0 }; }
export async function fetchOpsReport(id: number): Promise<OpsReport> { const res = await request(`${getApiBase()}/ops-reports/${id}`, undefined, "fetch ops report failed"); const data = await res.json(); return data?.report; }
// 按需生成升级建议（LLM 调用，最长 ~120s，放宽超时）
export async function suggestOpsReport(): Promise<OpsReport> { const res = await request(`${getApiBase()}/ops-reports/suggest`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }, "generate ops suggestions failed", 130_000); const data = await res.json(); return data?.report; }
// 一键转任务卡：按建议的 suggested_card 建看板卡（source=ops_suggestion）；high 风险建议会被后端拒绝
export async function createOpsSuggestionCard(reportId: number, idx: number, listId: number, assignee = ""): Promise<BoardCard> { const res = await request(`${getApiBase()}/ops-reports/${reportId}/suggestions/${idx}/card`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ list_id: listId, assignee }) }, "create ops suggestion card failed"); return res.json(); }

// ---- S7 产品情报：产品 Agent（对话 / 文档 / 拆解；docs/plan/S7-产品情报.md）----
export interface ProductSourceRef { doc_title: string; source_type: string; confidentiality: string; updated_at?: string; snippet?: string }
export interface ProductChatResult { answer: string; sources: ProductSourceRef[]; found: boolean }
export interface ProductDoc { id: number; board_id: number; title: string; doc_type: string; content: string; sources: any; card_id?: number | null; status: string; created_at: string }
export interface ProductCardResult { card_id: number; title: string; status: string; evolve_task_no?: string; work_item_no?: string }
// RAG 接地对话（LLM 调用，最长 ~90s，放宽超时）
export async function productChat(query: string, k = 5): Promise<ProductChatResult> { const res = await request(`${getApiBase()}/product/chat`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ query, k }) }, "product chat failed", 95_000); return res.json(); }
// 生成产品文档/变更需求（变更需求五要素缺项会被后端打回 422；最长 ~120s）
export async function productGenerateDoc(boardId: number, prompt: string, docType: string): Promise<ProductDoc> { const res = await request(`${getApiBase()}/product/doc`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ board_id: boardId, prompt, doc_type: docType }) }, "generate product doc failed", 130_000); return res.json(); }
// 对话/会议纪要拆解为任务卡（JSON Schema 校验，漂移即失败；最长 ~120s）
export async function productDecompose(boardId: number, listId: number, text: string): Promise<ProductCardResult[]> { const res = await request(`${getApiBase()}/product/decompose`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ board_id: boardId, list_id: listId, text }) }, "decompose failed", DECOMPOSE_TIMEOUT_MS); const data = await res.json(); return data?.cards ?? []; }
export async function fetchProductDocs(boardId: number, status = ""): Promise<ProductDoc[]> { const url = new URL(`${getApiBase()}/product/docs`); url.searchParams.set("board_id", String(boardId)); if (status) url.searchParams.set("status", status); const res = await request(url.toString(), undefined, "fetch product docs failed"); const data = await res.json(); return data?.docs ?? []; }
export async function setProductDocStatus(id: number, status: string): Promise<void> { await request(`${getApiBase()}/product/docs/${id}/status`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ status }) }, "update product doc status failed"); }
export async function updateProductDoc(id: number, content: string): Promise<void> { await request(`${getApiBase()}/product/docs/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ content }) }, "update product doc failed"); }

// ---- S9 统一工作项（docs/plan/S9-统一工作项流水线.md §8）----
// 需求 / Bug / 自进化统一货币（work_items）+ 轻重缓急队列（score + P0-P3）。
export interface WorkItem {
  id: number; no: string; kind: string; title: string; description: string;
  source: string; source_ref: string; target_name: string;
  score: number; priority: string; severity: number; urgency: number; impact: number; frequency: number;
  status: string; due_at?: string | null; checklist: string; assignee: string;
  locked: boolean; triage_degraded: boolean; card_id?: number | null; evolve_task_no: string;
  created_at: string; triaged_at?: string | null; picked_at?: string | null; shipped_at?: string | null; updated_at: string;
}
export interface WorkItemFilter { kind?: string; source?: string; status?: string; min_score?: number }
export async function fetchWorkItems(f: WorkItemFilter = {}): Promise<WorkItem[]> {
  const url = new URL(`${getApiBase()}/work-items`);
  if (f.kind) url.searchParams.set("kind", f.kind);
  if (f.source) url.searchParams.set("source", f.source);
  if (f.status) url.searchParams.set("status", f.status);
  if (f.min_score) url.searchParams.set("min_score", String(f.min_score));
  const res = await request(url.toString(), undefined, "fetch work items failed");
  const data = await res.json(); return data?.items ?? [];
}
export async function confirmWorkItem(no: string): Promise<void> { await request(`${getApiBase()}/work-items/${no}/confirm`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }, "confirm work item failed"); }
export async function rejectWorkItem(no: string, reason = ""): Promise<void> { await request(`${getApiBase()}/work-items/${no}/reject`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ reason }) }, "reject work item failed"); }
export async function setWorkItemLock(no: string, locked: boolean): Promise<void> { const ep = locked ? "lock" : "unlock"; await request(`${getApiBase()}/work-items/${no}/${ep}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }, "update work item lock failed"); }
export async function pickWorkItem(no: string): Promise<void> { await request(`${getApiBase()}/work-items/${no}/pick`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }, "pick work item failed", 30_000); }
export async function triggerWorkTriage(window = "168h"): Promise<any> { const res = await request(`${getApiBase()}/work-items/triage`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ window }) }, "trigger triage failed", 130_000); return res.json(); }

// ---- S9 §7.8 项目注册（apps/ 目录 ↔ 项目 ↔ 工作项可执行目标）----
export interface Project {
  id: number; name: string; dir: string; runtime: string;
  image: string; test_cmd: string; description: string; active: boolean;
  registered_at: string; updated_at: string;
}
export async function fetchProjects(): Promise<Project[]> {
  const res = await request(`${getApiBase()}/projects`, undefined, "fetch projects failed");
  const data = await res.json(); return data?.projects ?? [];
}
export async function scanProjects(): Promise<{ scanned: number; created: number; updated: number }> {
  const res = await request(`${getApiBase()}/projects/scan`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }, "scan projects failed", 60_000);
  return res.json();
}
export async function updateProject(id: number, data: Record<string, any>): Promise<void> {
  await request(`${getApiBase()}/projects/${id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "update project failed");
}
export async function deleteProject(id: number): Promise<void> {
  await request(`${getApiBase()}/projects/${id}`, { method: "DELETE" }, "delete project failed");
}

// ---- S9 卡片执行记录（每次 hand-off 一个工作项 = 一轮 S1 链路，成功失败可查看）----
export interface ExecutionEvent { action: string; actor: string; at: string; detail: string; note: string }
export interface CardExecution {
  work_item_no: string; kind: string; status: string; title: string;
  target_name: string;
  created_at: string; picked_at?: string | null; shipped_at?: string | null;
  evolve_task_no: string; task_status: string; task_updated_at?: string | null; task_review: string;
  task_diff: string; branch: string; push_cmd: string; compare_url: string;
  merge_commit: string; pr_url: string; pr_error?: string; coder_session: string;
  /** 合入阻塞态："" = 正常；paused_base_dirty = 相关仓库（主/子）有未提交改动 → 合入暂停（看板显示阻塞） */
  task_merge_state: string;
  events: ExecutionEvent[];
}
export async function fetchCardExecutions(cardId: number): Promise<CardExecution[]> {
  const res = await request(`${getApiBase()}/cards/${cardId}/executions`, undefined, "fetch card executions failed");
  const data = await res.json(); return data?.executions ?? [];
}
// S9 看板直接审批（Review 列）：卡 → 在审工作项 → 进化单 → 审批单（复用 S1 审批链，运维中心不变）
export async function approveCardApproval(cardId: number, reason = ""): Promise<{ ok?: boolean; note?: string }> {
  // 不用 request 的 errorMsg（那样只会得到 "approve card approval failed: 404"，吞掉后端可读文案）。
  // 后端行为：有待审批单 → 走审批链；没有（「完成执行」推进 Review 的卡）→ 把进化单置 released
  // 等 driver 合入 main，合入后才进 Done（红线：代码必须合并才能 Done）。
  const res = await request(`${getApiBase()}/cards/${cardId}/approve`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ reason }) }, undefined, 30_000);
  if (!res.ok) {
    const data = await res.json().catch(() => null);
    throw new Error(data?.error || `审批失败（HTTP ${res.status}）`);
  }
  return res.json().catch(() => ({}));
}
export async function rejectCardApproval(cardId: number, reason = ""): Promise<void> {
  const res = await request(`${getApiBase()}/cards/${cardId}/reject`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ reason }) }, undefined, 30_000);
  if (!res.ok) {
    const data = await res.json().catch(() => null);
    throw new Error(data?.error || `驳回失败（HTTP ${res.status}）`);
  }
}

// ---- 产品「文档」：仓库 docs/ 目录（树形浏览 / 编辑 / 预览 + AI 生成，2026-10-19）----
// 与 S7 产品情报的 knowledge_docs（DB + Qdrant）不是一回事：这里直接读写仓库 docs/ 文件。
export interface DocNode { name: string; path: string; type: "file" | "dir"; size: number; mtime: string; children?: DocNode[] }
export interface DocFile { path: string; content: string; size: number; mtime: string; binary: boolean; editable: boolean }
export async function fetchDocTree(): Promise<DocNode> {
  const res = await request(`${getApiBase()}/docs/tree`, undefined, "fetch docs tree failed", 30_000);
  const data = await res.json();
  if (!data?.tree) throw new Error("fetch docs tree failed: 响应缺少 tree 字段");
  return data.tree as DocNode;
}
export async function fetchDocFile(path: string): Promise<DocFile> {
  const url = new URL(`${getApiBase()}/docs/file`);
  url.searchParams.set("path", path);
  const res = await request(url.toString(), undefined, "fetch doc failed", 30_000);
  const data = await res.json();
  if (!data?.file) throw new Error("fetch doc failed: 响应缺少 file 字段");
  return data.file as DocFile;
}
export async function saveDocFile(path: string, content: string): Promise<DocFile> {
  const res = await request(`${getApiBase()}/docs/file`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ path, content }) }, undefined, 60_000);
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error || `保存失败（HTTP ${res.status}）`);
  return data?.file as DocFile;
}
export async function createDocNode(path: string, dir = false): Promise<void> {
  const res = await request(`${getApiBase()}/docs/file`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ path, dir }) }, undefined, 30_000);
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error || `新建失败（HTTP ${res.status}）`);
}
export async function deleteDocNode(path: string): Promise<void> {
  const url = new URL(`${getApiBase()}/docs/file`);
  url.searchParams.set("path", path);
  const res = await request(url.toString(), { method: "DELETE" }, undefined, 30_000);
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error || `删除失败（HTTP ${res.status}）`);
}
export async function renameDocNode(from: string, to: string): Promise<void> {
  const res = await request(`${getApiBase()}/docs/rename`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ from, to }) }, undefined, 30_000);
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error || `重命名失败（HTTP ${res.status}）`);
}
// fetchDocRawURL 取预览用短时签名 URL（HTML 相对引用靠它加载图片/CSS）；返回可直接塞进 iframe/img 的绝对 URL。
export async function fetchDocRawURL(path: string): Promise<string> {
  const res = await request(`${getApiBase()}/docs/raw-url`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ path }) }, undefined, 30_000);
  const data = await res.json().catch(() => null);
  if (!res.ok || !data?.url) throw new Error(data?.error || `取预览链接失败（HTTP ${res.status}）`);
  return `${getApiBase()}${data.url}`;
}
// aiDoc AI 生成/改写文档正文（不落盘：返回内容由调用方决定保存到哪）。
export async function aiDoc(mode: "generate" | "edit", prompt: string, path = ""): Promise<string> {
  const res = await request(`${getApiBase()}/docs/ai`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ mode, prompt, path }) }, undefined, 180_000);
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error || `AI 生成失败（HTTP ${res.status}）`);
  return (data?.content ?? "") as string;
}

// ---- S5 运维中心「运行服务总览」（wi-1792321102）----
// 服务列表/详情/重启/停止/启动/回滚/改参。写操作需 admin 角色（后端 requireRole），
// 不纳管（external）服务后端拒绝写操作（403）。
export const DEFAULT_LOG_TAIL = 200;
export interface ServiceView {
  name: string;
  container: string;
  image: string;
  state: string; // running | exited | paused | restarting | dead | not_created
  status: string;
  health: string; // healthy | unhealthy | starting | ""
  ports: string[];
  started_at: string;
  managed: boolean;
  start_method: string; // compose | dynamic | external
  one_shot: boolean;
  exit_code: number;
}
export interface ServiceVersionEntry {
  version: string;
  image: string;
  status: string;
  created_at: string;
}
export interface ServiceDetail extends ServiceView {
  logs: string;
  compose_file: string;
  environment: Record<string, string> | null;
  volumes: string[] | null;
  networks: string[] | null;
  restart: string;
  health_check: { test: string[]; interval: string; timeout: string; retries: number; start_period: string } | null;
  versions: ServiceVersionEntry[];
  asset_profile: string;
}
export async function fetchServices(): Promise<ServiceView[]> {
  const res = await request(`${getApiBase()}/services`, undefined, "fetch services failed", 30_000);
  const data = await res.json();
  if (!Array.isArray(data?.services)) throw new Error("fetch services failed: 响应缺少 services 字段");
  return data.services;
}
export async function fetchServiceDetail(name: string, tail = DEFAULT_LOG_TAIL): Promise<ServiceDetail> {
  const res = await request(`${getApiBase()}/services/${encodeURIComponent(name)}?tail=${tail}`, undefined, "fetch service detail failed", 30_000);
  const data = await res.json();
  if (!data?.service) throw new Error(`fetch service detail failed: 响应缺少 service 字段`);
  return data.service as ServiceDetail;
}
export async function restartService(name: string): Promise<void> {
  await request(`${getApiBase()}/services/${encodeURIComponent(name)}/restart`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }, "restart service failed", 60_000);
}
export async function stopService(name: string): Promise<void> {
  await request(`${getApiBase()}/services/${encodeURIComponent(name)}/stop`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }, "stop service failed", 60_000);
}
export async function startService(name: string): Promise<void> {
  await request(`${getApiBase()}/services/${encodeURIComponent(name)}/start`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }, "start service failed", 60_000);
}
export async function rollbackService(name: string, image: string): Promise<void> {
  await request(`${getApiBase()}/services/${encodeURIComponent(name)}/rollback`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ image }) }, "rollback service failed", 120_000);
}
export async function updateServiceParams(name: string, data: { env?: Record<string, string>; ports?: string[]; reason?: string }): Promise<void> {
  await request(`${getApiBase()}/services/${encodeURIComponent(name)}/params`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, "update service params failed", 120_000);
}

// S9 失败重跑（看板按钮，2026-10-18）：resume=续接执行（同进化单/worktree/coder 会话/已积累 diff），
// restart=重新执行（新工作项 + 干净 worktree + 全新会话，从零开始）。
// 不用 request 的 errorMsg（会丢服务端 409 的可读原因），改为透出响应体的 error 文案。
export interface RerunResult { mode: string; work_item_no: string; evolve_task_no: string; task_status: string; note?: string }
export async function rerunCard(cardId: number, mode: "resume" | "restart"): Promise<RerunResult> {
  const res = await request(`${getApiBase()}/cards/${cardId}/rerun`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ mode }) }, undefined, 30_000);
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error(data?.error || `重跑失败（HTTP ${res.status}）`);
  return (data?.result ?? data) as RerunResult;
}
