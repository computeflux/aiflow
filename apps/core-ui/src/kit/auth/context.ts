import { createContext } from "react";

// board-kit auth context（独立 .ts：组件文件只导出组件，context 供 hooks/useAuth 与 auth.tsx 共享）
export interface AuthUserFromSession {
  verifierId?: string;
  email?: string;
  name?: string;
}

export interface AuthContextValue {
  isInitialized: boolean;
  isConnected: boolean;
  isConnecting: boolean;
  connectError: Error | null;
  user: { id: string; email?: string; name?: string; roles: string[] } | null;
  /** 跳转枢纽 /login 获取登录凭证（本服务不弹登录框） */
  login: () => Promise<void>;
  /** 登出（调 auth /logout + 清本地会话） */
  logout: () => Promise<void>;
  /** 当前 access token（未登录为空） */
  token: () => string;
  /** 当前 access token（未登录为空；同 origin 可静默续期） */
  refresh: () => Promise<string | null>;
}

export const AuthContext = createContext<AuthContextValue | null>(null);
