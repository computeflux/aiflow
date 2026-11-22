#!/bin/bash
# 导出符号 doc comment 检查（Google Go style guide「Comments」：导出符号必须有 doc comment）。
# 用法：bash apps/core/hacks/doccheck.sh [目录...]（默认 apps/core 的 cmd + internal）
# 退出码非 0 = 存在缺 doc 的导出顶层符号（func/type/const/var；不含结构体字段，字段用行内注释）。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TOOL_DIR="$(mktemp -d /tmp/doccheck.XXXXXX)"
trap 'rm -rf "$TOOL_DIR"' EXIT

cat > "$TOOL_DIR/main.go" <<'EOF'
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	count := 0
	for _, root := range os.Args[1:] {
		filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
			if err != nil {
				return nil
			}
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					if d.Recv != nil || !d.Name.IsExported() {
						continue
					}
					if d.Doc == nil {
						fmt.Printf("%s:%d: func %s\n", p, fset.Position(d.Pos()).Line, d.Name.Name)
						count++
					}
				case *ast.GenDecl:
					if d.Tok == token.IMPORT {
						continue
					}
					for _, s := range d.Specs {
						var name string
						switch s := s.(type) {
						case *ast.TypeSpec:
							name = s.Name.Name
						case *ast.ValueSpec:
							if len(s.Names) > 0 {
								name = s.Names[0].Name
							}
						}
						if name == "" || !ast.IsExported(name) {
							continue
						}
						if d.Doc == nil {
							fmt.Printf("%s:%d: %s %s\n", p, fset.Position(s.Pos()).Line, d.Tok, name)
							count++
						}
					}
				}
			}
			return nil
		})
	}
	fmt.Println("total missing doc:", count)
	if count > 0 {
		os.Exit(1)
	}
}
EOF

cd "$TOOL_DIR"
go mod init doccheck >/dev/null 2>&1

CORE_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
if [ $# -eq 0 ]; then
  set -- "$CORE_ROOT/cmd" "$CORE_ROOT/internal"
fi
go run . "$@"
