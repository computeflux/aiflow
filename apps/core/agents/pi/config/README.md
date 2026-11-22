# pi 本地配置（gitignore 保护）

- `auth.json`：pi CLI 的模型凭据（如 `google`/`deepseek`/`anthropic` 的 API key），由你在仓库内手工填写。**会被 `.gitignore` 忽略，切勿提交**。
- `env`：pi-web 服务的 Basic Auth 凭据（`PI_WEB_PASSWORD` / `PI_WEB_HOSTNAME`），由部署脚本生成，`systemctl --user` 单元通过 EnvironmentFile 读取。
- 会话目录：默认取脚本所在目录 `agents/pi`（`PI_SESSION_DIR` 可改），`auth.json` 与 `models-store.json` 也存于此。会话文件（`*.jsonl`、`sessions/`）会被 `.gitignore` 忽略。

凭据示例（勿提交）：

```
PI_WEB_PASSWORD=<随机长密码>
PI_WEB_HOSTNAME=127.0.0.1
```
