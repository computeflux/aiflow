#!/bin/bash
# 全量 CI（S1，docs/plan/S1-进化闭环.md §3.6）：
#   gofmt + 导出符号 doc comment 检查（Google style 基线，hacks/doccheck.sh）
#   go build ./cmd/... ./internal/...（契约：接口签名可编译）+ go test 同范围（主模块，apps/core）
#   注：不用 ./... —— deploy/Dockerfile.go 会被当 Go 源解析；-buildvcs=false（只读挂载无 git 状态）
#   + .venv/python/bin/python -m pytest（apps/ 下每个 python 项目；.venv + 自包含 uv python 树
#     由 evolve-driver syncVenvs 拷入 worktree；各出一份 JUnit XML）
#
# 输入：$1 = task_no
# 输出：
#   JUnit XML → $CI_OUT_DIR/<task_no>-<target>.xml
#   stdout 末行 JSON 摘要（evolve-driver 解析）：
#   {"junit":"...","passed":N,"failed":M,"contract":"ok|fail"}
#
# 运行环境：ci-runner 容器（deploy/Dockerfile.ci），仓库 worktree 挂 /work（只读）。
set -uo pipefail

TASK_NO="${1:?usage: ci-runner.sh <task_no>}"
REPO="${CI_REPO_DIR:-/work}"
OUT_DIR="${CI_OUT_DIR:-/cache/ci}"
mkdir -p "$OUT_DIR"

CONTRACT="ok"
FAILED=0
NO_TESTS=""

cd "$REPO"
# 主 Go 模块已迁 apps/core（2026-10-10 仓库重组）；worktree 内路径为 apps/core/go.mod
if [ -f apps/core/go.mod ]; then
  cd apps/core
  # Google style 基线（2026-11-18 结构治理接入）：gofmt + 导出符号 doc comment。
  # 计入 contract（失败置 CONTRACT=fail），不进 go test 计数体系；存量已全量补齐（基线零告警）。
  echo ">>> gofmt -l cmd internal（格式基线）"
  if [ -n "$(gofmt -l cmd internal)" ]; then
    echo ">>> gofmt: 上述文件未格式化（契约失败）"
    FAILED=1
    CONTRACT="fail"
  fi
  echo ">>> hacks/doccheck.sh（导出符号 doc comment 基线）"
  if ! bash hacks/doccheck.sh; then
    FAILED=1
    CONTRACT="fail"
  fi
  echo ">>> go build ./cmd/... ./internal/... (contract: 接口签名可编译)"
  if go build -buildvcs=false ./cmd/... ./internal/...; then
    echo ">>> go test ./cmd/... ./internal/..."
    go mod download
    # gotestsum 自己跑 go test（-json 由它注入）；包路径作为参数传入（不能管道喂 stdin）
    if gotestsum --junitfile "$OUT_DIR/$TASK_NO-go.xml" -- -buildvcs=false ./cmd/... ./internal/...; then
      :
    else
      FAILED=1
    fi
  else
    FAILED=1
    CONTRACT="fail"
  fi
fi

cd "$REPO"
for d in "$REPO"/apps/*/; do
  [ -f "$d/pyproject.toml" ] || continue
  	# 无测试文件的项目跳过（pytest exit 5 = no tests collected，不算失败）
  	# 排除 .venv（宿主挂载场景下会误匹配 site-packages 测试）；
  	# -print -quit 首中即退，避免 grep -q 关管道 → find SIGPIPE → pipefail 误判
  	# 注：括号转义在 $( ) 内必须写 \( \)——\\( 会被解析成“转义反斜杠 + 裸左括号”
  	# 而报 syntax error（2026-09-30 实测：脚本在 python 环节前即中断，摘要 JSON 从未输出）。
  	if [ -z "$(find "$d" -name ".venv" -prune -o \( -name "test_*.py" -o -name "conftest.py" \) -print -quit 2>/dev/null)" ]; then
    echo ">>> $d: no tests, skip"
    continue
  fi
  name="$(basename "$d")"
  # .venv 缺失 = 无法验证该应用测试 → 失败（不放行未验证的变更）
  if [ ! -x "$d/.venv/python/bin/python" ]; then
    echo ">>> $d: no .venv, cannot run python tests"
    FAILED=1
    continue
  fi
  	echo ">>> .venv/python/bin/python -m pytest $name"
  	SP="$(ls -d "$d"/.venv/lib/python3.*/site-packages 2>/dev/null | head -1)"
  	# PYTEST_ADDOPTS=-p no:cacheprovider：/work 是真只读挂载（2026-11 实测 CI 容器内
  	# .pytest_cache 写入报 Errno 30 Read-only file system），禁用 cacheprovider 避免尾写盘失败。
  	if (cd "$d" && PYTEST_ADDOPTS="-p no:cacheprovider" PYTHONHOME="$PWD/.venv/python" PYTHONPATH="$SP" .venv/python/bin/python -m pytest --junitxml "$OUT_DIR/$TASK_NO-$name.xml" -q); then
  		:
  	else
  		rc=$?
  		if [ "$rc" -eq 5 ]; then
  			# exit 5 = 未收集到用例（如 pycraper/tests/*.py 是手工脚本而非 pytest 用例）：
  			# 按本脚本约定不算失败，但必须留痕（摘要 no_tests + stderr），避免“测试没跑还全绿”。
  			echo ">>> $name: pytest collected 0 tests (exit 5) → 记入 no_tests，不计失败" >&2
  			NO_TESTS="$NO_TESTS $name"
  		else
  			FAILED=1
  		fi
  	fi
done

# 汇总 JUnit（只统计叶子 testsuite，避免 gotestsum 嵌套重复计数）
CI_CONTRACT="$CONTRACT" CI_FAILED="$FAILED" CI_NO_TESTS="$NO_TESTS" python3 - "$OUT_DIR" "$TASK_NO" <<'PY'
import glob, json, os, sys
import xml.etree.ElementTree as ET
out_dir, task_no = sys.argv[1], sys.argv[2]
tests = failures = skipped = 0
files = []
for f in sorted(glob.glob(os.path.join(out_dir, task_no + "-*.xml"))):
    files.append(f)
    try:
        root = ET.parse(f).getroot()
    except ET.ParseError:
        continue
    for suite in root.iter("testsuite"):
        if suite.find("testsuite") is not None:
            continue  # 非叶子（汇总节点），跳过
        tests += int(suite.get("tests", "0"))
        failures += int(suite.get("failures", "0")) + int(suite.get("errors", "0"))
        skipped += int(suite.get("skipped", "0"))
contract = "fail" if (os.environ.get("CI_CONTRACT") == "fail" or os.environ.get("CI_FAILED") == "1") else "ok"
print(json.dumps({
    "junit": " ".join(files),
    "passed": tests - failures - skipped,
    "failed": failures,
    "contract": contract,
    "no_tests": [x for x in os.environ.get("CI_NO_TESTS", "").split() if x],
}))
PY

exit "$FAILED"
