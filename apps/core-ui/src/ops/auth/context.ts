import { createContext } from "react";

// 枢纽 web3auth auth context（独立 .ts：组件文件只导出组件，context 供 hooks/useAuth 与 auth.tsx 共享）
export interface AuthContextValue {
  isInitialized: boolean;
  isConnected: boolean;
  isConnecting: boolean;
  connectError: Error | null;
  user: { id: string; email?: string; name?: string; roles: string[] } | null;
  /** 打开登录弹窗（仅 /login 页使用；其他页面一律 redirectToLogin 跳登录页，不能就地登录） */
  login: () => Promise<void>;
  /** 登出（调 auth /logout + 清本地会话） */
  logout: () => Promise<void>;
  /** 当前 access token（未登录为空） */
  token: () => string;
  /** 401 时静默续期：成功返回新 token，失败返回 null（调用方应跳登录） */
  refresh: () => Promise<string | null>;
}

export const AuthContext = createContext<AuthContextValue | null>(null);
