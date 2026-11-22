package main

// audit tool: list exported top-level Func/Type/Const/Var without a doc comment.

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
	root := os.Args[1]
	count := 0
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
	fmt.Println("total missing doc:", count)
}
