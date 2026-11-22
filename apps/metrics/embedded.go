package main

import "embed"

// 前端静态资源(web/dist, 由 `cd web && npm run build` 生成)。
// 构建 sslmon 前需先构建前端; 部署时单二进制即可, 无需携带 dist 目录。
//
//go:embed web/dist
var embeddedDist embed.FS
