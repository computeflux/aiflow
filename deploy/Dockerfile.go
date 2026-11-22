# syntax=docker/dockerfile:1
# ComputeFlux workflow — Go 服务统一镜像（多阶段 golang:1.26 → distroless）
#
# 用法（在 deploy/ 下）：
#   docker compose up -d --build <svc>
# 或手工：
#   docker build -f deploy/Dockerfile.go --build-arg target=api <repo-root>/apps/core
#
# ARG target 选择入口（一镜像一入口）：
#   api / go-worker / generate-skill / evaluate-skill / search-skill / skillctl
#   audit / sandbox-runner / agentctl（S1 进化闭环，deploy/compose/evolve/ + tools/）
#   flex-server（S2 柔性分支图引擎，:8124 仅内部网络）
#   watchdog（S3 干预舱死循环检测，1 分钟 ticker，:8125 仅内部网络）
#   matrix-bot（S4 协作底座审批 Bot，/sync 长轮询，:8126 仅内部网络）
#   ops-report（S6 运维情报，daily/weekly ticker，:8128 仅内部网络）
#   knowledge-sync（S7 产品情报汇聚，daily 02:00 ticker，:8129 仅内部网络）
#   work-dispatch（S9 统一工作项流水线，分诊/调度/回流三循环，:8130 仅内部网络）
#   mk-judge（营销判断服务 HTTP 入口，:8131 仅内部网络）
#   inventory（S5 软件资产画像，tools profile 一次性）
# 例外：deployctl / health-patrol 需 docker CLI + shell 驱动宿主 daemon
#（deploy/Dockerfile.deploy）；evolve-driver 需 git 建 worktree（deploy/Dockerfile.evolve）；
# ocr / ci-runner 需 node / go+python 工具链（deploy/Dockerfile.ocr / .ci）。

FROM golang:1.26 AS builder
WORKDIR /src
ARG target
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN case "$target" in \
      api) pkg="./cmd/api" ;; \
      go-worker) pkg="./cmd/workflow" ;; \
      generate-skill|evaluate-skill|search-skill|skillctl) pkg="./cmd/$target" ;; \
      audit|sandbox-runner|agentctl|flex-server|watchdog|matrix-bot) pkg="./cmd/$target" ;; \
      ops-report) pkg="./cmd/ops-report" ;; \
      knowledge-sync) pkg="./cmd/knowledge-sync" ;; \
      work-dispatch) pkg="./cmd/work-dispatch" ;; \
      mk-judge) pkg="./cmd/mk-judge" ;; \
      inventory) pkg="./cmd/inventory" ;; \
      *) echo "unknown target: $target" && exit 1 ;; \
    esac \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "$pkg"

# hc：最小健康检查探针（独立小模块，stdlib only，apps/core/hc 自带 go.mod）
FROM golang:1.26 AS hcbuilder
WORKDIR /hc
COPY hc/go.mod hc/main.go ./
RUN go build -o /hc/hc .

FROM gcr.io/distroless/base
COPY --from=builder /out/app /bin/app
COPY --from=hcbuilder /hc/hc /bin/hc
USER nonroot
ENTRYPOINT ["/bin/app"]
