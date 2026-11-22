import { useEffect, useRef } from "react";
import { LogIn } from "lucide-react";
import { useAuth } from "../hooks";
import { isHubLoginPage, readStoredSession, returnUrlWithSession, takeUrlParam } from "../sso";

// 登录页：全系统唯一的登录入口（新设计：只在这页弹登录框，所有服务都来这里获取登录凭证）。
//  - 进入时未登录 → 自动打开 web3auth LoginModal
//  - 已登录且带 ?sso_next= → 先轮换出新 access token（枢纽是唯一合法轮换者，
//    见 board-kit/src/api.ts 注释）再带会话跳回原服务（returnUrlWithSession）；
//    无 ?sso_next= → 回任务控制台 /
//  - 会话已失效（refresh 失败）→ 清会话，自动重弹登录框
//  - 用户关闭弹窗未登录 → 可点下方“登录”按钮重新弹框
export function Login() {
  const { isInitialized, isConnected, login, refresh, logout } = useAuth();
  // 挂载即消费 sso_next（SsoGate 不再读该参数，回跳由本页负责）
  const ssoNext = useRef<string | null>(takeUrlParam("sso_next"));
  const prompted = useRef(false);
  const loginRef = useRef(login);
  loginRef.current = login;

  useEffect(() => {
    if (isInitialized && !isConnected && !prompted.current) {
      prompted.current = true;
      void loginRef.current();
    }
  }, [isInitialized, isConnected]);

  useEffect(() => {
    if (!isConnected) return;
    let cancelled = false;
    void (async () => {
      // 枢纽 origin（core-ui :5181）：localStorage 会话就是权威会话，无需
      // refresh 轮换（且 StrictMode/重渲染双 effect 并发 refresh 会触发重放
      // 检测吊销会话），直接回跳。
      // 跨 origin 服务（iframe 等）才需要在此换新 token（本组件不会发生，
      // 保留分支以防登录页被复用到其他 origin）。
      const tok = isHubLoginPage() ? readStoredSession()?.token ?? null : await refresh();
      if (cancelled) return;
      if (!tok) {
        // 会话已失效（refresh 被吊销/过期）→ 清会话，上面的 effect 重弹登录框
        await logout();
        return;
      }
      // 无 ?sso_next= → 回 ops 入口首页（单容器 /ops/ 路径，见 core-ui.conf）
      window.location.replace(ssoNext.current ? returnUrlWithSession(ssoNext.current) : "/ops/");
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isConnected]);

  return (
    <div className="h-screen flex flex-col items-center justify-center gap-5">
      {/* 单入口 SPA：public 资源在站点根，用绝对路径 */}
      <img src="/logo-mini.svg" alt="logo" className="h-10 w-auto" />
      <div className="text-lg font-medium text-(--text-primary)">workflow 登录</div>
      <div className="text-xs text-(--text-muted)">登录后自动返回原页面</div>
      <button
        type="button"
        onClick={() => void loginRef.current()}
        className="flex items-center gap-2 px-6 py-2.5 text-sm font-medium bg-white text-black rounded-none hover:opacity-90 transition-opacity cursor-pointer"
      >
        <LogIn className="h-4 w-4" /> 登录
      </button>
    </div>
  );
}
