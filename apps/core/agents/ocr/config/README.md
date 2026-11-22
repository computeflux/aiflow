# ocr 本地配置（gitignore 保护）

open-code-review 的 LLM 端点/模型/凭据由 `ocr config` 子命令管理，通常写入
`~/.ocr/`（或本目录），均不提交。

首次使用：

```bash
ocr config provider   # 配置 LLM 端点（与 .env 中 LLM 配置一致即可）
ocr config model
```
