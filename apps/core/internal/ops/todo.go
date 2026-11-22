package ops

import (
	"bufio"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// 扫描限额（避免拖慢周报）：最多读 maxFiles 个文本文件，单文件 ≤ maxBytes。
const (
	maxFiles = 4000
	maxBytes = 1 << 20 // 1MB
)

// todoScan 是 TODO/FIXME 扫描结果。
type todoScan struct {
	Total   int
	Samples []string // 形如 "path:line: 文本"，上限 30 条
}

// ScanTODO 扫描给定目录下代码中的 TODO/FIXME（S6 §3.2 输入之一）。
// 只扫文本文件，跳过 .git / 依赖目录 / 大文件，结果有上限，避免拖慢周报。
func ScanTODO(ctx context.Context, repoRoot string, dirs []string) todoScan {
	res := todoScan{}
	seenFiles := 0
	for _, d := range dirs {
		root := filepath.Join(repoRoot, d)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return fs.SkipAll
			}
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if isSkipDir(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			if !isCodeFile(d.Name()) {
				return nil
			}
			if seenFiles >= maxFiles {
				return fs.SkipAll
			}
			seenFiles++
			scanFile(path, &res)
			return nil
		})
	}
	return res
}

func isSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build", ".venv", "__pycache__", "target":
		return true
	}
	return false
}

func isCodeFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".vue", ".sh", ".sql", ".md":
		return true
	}
	return false
}

func scanFile(path string, res *todoScan) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() > maxBytes {
		return
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if strings.Contains(line, "TODO") || strings.Contains(line, "FIXME") {
			res.Total++
			if len(res.Samples) < 30 {
				res.Samples = append(res.Samples, strings.TrimSpace(line))
			}
		}
	}
}
