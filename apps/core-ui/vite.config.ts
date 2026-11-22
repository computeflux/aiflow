import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.dirname(fileURLToPath(import.meta.url))

// 单入口 React 项目（:5181 单容器单端口）：
// /ops /product /content /email 四个板块由 src/main.tsx 的路由区分，
// 按路由懒加载做代码分割；nginx SPA 回退见 deploy/nginx/core-ui.conf。
export default defineConfig({
  plugins: [react(), tailwindcss()],
  // @wetee/web3auth 是 file: 源码依赖（main 指向 src/*.ts），
  // 其内部 import 的 react/react-router-dom 等必须从本项目的 node_modules 解析：
  // 自身 node_modules 里若存在同名包（npm 自动装 peer）会产生第二份实例，
  // 两个 Router/React 上下文互不相通 → useLocation() 报错。dedupe 强制单实例。
  // @kit 是项目内共享代码（src/kit，原 board-kit）。
  resolve: {
    dedupe: ['react', 'react-dom', 'react-router-dom', 'react-router', 'lucide-react', 'jodit-react', 'jodit'],
    // 双实例红线：@wetee/web3auth 是 file: 符号链接依赖，各自 node_modules
    // 里可能有第二份 react（版本相同也是独立模块实例）→ createContext/useContext
    // 跨实例不通 → "useAuth must be used within AuthProvider"。
    // dedupe 对链接依赖不可靠，用 alias 显式钉死到本项目 node_modules 的单一实例。
    alias: [
      { find: /^react-dom$/, replacement: path.resolve(root, 'node_modules/react-dom') },
      { find: /^react$/, replacement: path.resolve(root, 'node_modules/react') },
      { find: '@kit', replacement: path.resolve(root, 'src/kit/index.ts') },
    ],
  },
  server: {
    port: 5181,
    host: '0.0.0.0',
    allowedHosts: ['dev-workflow.computeflux.ai', 'xiaobai.asyou.me'],
  },
})
