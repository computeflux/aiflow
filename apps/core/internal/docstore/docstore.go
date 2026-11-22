// Package docstore 提供仓库 docs/ 目录的文件树读写（产品板块「文档」功能的存储层）。
//
// 安全红线（都是硬约束，不依赖调用方自觉）：
//   - 路径归一后必须落在 docs 根内（拒绝 `..` 逃逸 / 绝对路径 / 空路径 / NUL）；
//   - 已存在路径额外做 EvalSymlinks 校验，防符号链接逃出根目录；
//   - 写入只允许文本扩展名白名单（防误写二进制/覆盖图片）；
//   - 单文件读写上限 MaxEditBytes（防大文件灌爆编辑器与内存）。
//
// 目录树跳过 .git / node_modules / .DS_Store；深度上限 MaxTreeDepth（防病态嵌套）。
package docstore

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 文档读写的安全上限。
const (
	// MaxEditBytes 单文件读写上限（编辑器场景；更大的文件走 raw 预览）。
	MaxEditBytes = 2 << 20
	// MaxTreeDepth 目录树递归深度上限。
	MaxTreeDepth = 12
)

// editableExt 可写入的文本扩展名白名单（读不受限：非白名单一律标 binary 只读）。
var editableExt = map[string]bool{
	".md": true, ".markdown": true, ".html": true, ".htm": true,
	".css": true, ".js": true, ".mjs": true, ".json": true, ".txt": true,
	".yaml": true, ".yml": true, ".toml": true, ".sh": true, ".bash": true,
	".py": true, ".go": true, ".ts": true, ".tsx": true, ".jsx": true,
	".csv": true, ".svg": true, ".sql": true, ".conf": true, ".env": true,
}

// skip 目录/文件名跳过（不进树、不可写删）。
var skip = map[string]bool{".git": true, "node_modules": true, ".DS_Store": true}

// Store 是 docs 目录访问层（root = docs 绝对路径）。
type Store struct {
	root string
}

// New 创建访问层；root 为空返回 nil（调用方降级：端点 503）。
func New(root string) *Store {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	return &Store{root: filepath.Clean(root)}
}

// Root 返回配置的 docs 根目录。
func (s *Store) Root() string { return s.root }

// Node 是目录树节点（Path 是相对 docs 根的 URL 风格路径，始终用 `/`）。
type Node struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Type     string    `json:"type"` // file | dir
	Size     int64     `json:"size"`
	Mtime    time.Time `json:"mtime"`
	Children []Node    `json:"children,omitempty"`
}

// FileContent 是单个文件的内容与元信息。
type FileContent struct {
	Path     string    `json:"path"`
	Content  string    `json:"content"`
	Size     int64     `json:"size"`
	Mtime    time.Time `json:"mtime"`
	Binary   bool      `json:"binary"`   // 二进制/超大 → 内容不返回（前端用 raw 预览）
	Editable bool      `json:"editable"` // 文本白名单内且不超限
}

// resolve 把相对路径转成绝对路径并做全部安全校验（越界→错误）。
// 只接受**相对 docs 根**的路径：绝对路径与反斜杠（Windows 语义的穿越）一律拒绝，
// 不做“静默改写”（否则客户端传 /etc/passwd 会被当成 docs/etc/passwd，语义可疑）。
func (s *Store) resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("非法路径")
	}
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return "", fmt.Errorf("仅支持相对 docs 根的路径：%s", rel)
	}
	clean := path.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return "", fmt.Errorf("非法路径：%s", rel)
	}
	for _, seg := range strings.Split(clean, "/") {
		if skip[seg] {
			return "", fmt.Errorf("该路径不可访问：%s", rel)
		}
	}
	abs := filepath.Join(s.root, filepath.FromSlash(clean))
	rootClean := filepath.Clean(s.root)
	if abs != rootClean && !strings.HasPrefix(abs, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界：%s", rel)
	}
	// 已存在路径：解析符号链接后再校验一次（防软链逃逸）
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		if resolved != rootClean && !strings.HasPrefix(resolved, rootClean+string(filepath.Separator)) {
			return "", fmt.Errorf("路径越界（符号链接）：%s", rel)
		}
	}
	return abs, nil
}

// Rel 把绝对路径转回相对 docs 根的 `/` 风格路径。
func (s *Store) Rel(abs string) string {
	rel, err := filepath.Rel(filepath.Clean(s.root), abs)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// ResolveForRaw raw 服务的路径校验：允许任意扩展名（图片/附件），但必须在根内且是普通文件。
// 调用方传 gin 通配符（带前导 `/`）时先去掉一层。
func (s *Store) ResolveForRaw(rel string) (string, error) {
	rel = strings.TrimPrefix(rel, "/")
	abs, err := s.resolve(rel)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("文件不存在：%s", rel)
	}
	if st.IsDir() {
		return "", fmt.Errorf("目标是目录：%s", rel)
	}
	return abs, nil
}

// Tree 返回整个 docs 目录树（根节点名 "docs"）。
func (s *Store) Tree() (Node, error) {
	root, err := os.Stat(s.root)
	if err != nil {
		return Node{}, fmt.Errorf("docs 目录不可用（%s）：%w", s.root, err)
	}
	if !root.IsDir() {
		return Node{}, fmt.Errorf("docs 根不是目录：%s", s.root)
	}
	return s.walk(s.root, "", 0)
}

func (s *Store) walk(absDir, relDir string, depth int) (Node, error) {
	name := filepath.Base(absDir)
	if relDir == "" {
		name = "docs"
	}
	node := Node{Name: name, Path: relDir, Type: "dir"}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return node, nil // 单个目录不可读（权限）不阻断整棵树
	}
	children := make([]Node, 0, len(entries))
	for _, e := range entries {
		entryName := e.Name()
		if skip[entryName] {
			continue
		}
		rel := path.Join(relDir, entryName)
		abs := filepath.Join(absDir, entryName)
		if e.IsDir() {
			if depth+1 > MaxTreeDepth {
				continue
			}
			child, err := s.walk(abs, rel, depth+1)
			if err != nil {
				continue
			}
			children = append(children, child)
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		children = append(children, Node{
			Name: entryName, Path: rel, Type: "file", Size: info.Size(), Mtime: info.ModTime(),
		})
	}
	// 目录在前、同类型按名称排（VS Code 风格）
	sort.SliceStable(children, func(i, j int) bool {
		if (children[i].Type == "dir") != (children[j].Type == "dir") {
			return children[i].Type == "dir"
		}
		return strings.ToLower(children[i].Name) < strings.ToLower(children[j].Name)
	})
	node.Children = children
	if info, err := os.Stat(absDir); err == nil {
		node.Mtime = info.ModTime()
	}
	return node, nil
}

// Read 读取文件（文本白名单内且不超限才返回内容；否则标 binary 只读）。
func (s *Store) Read(rel string) (*FileContent, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("文件不存在：%s", rel)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("目标是目录：%s", rel)
	}
	ext := strings.ToLower(filepath.Ext(abs))
	fc := &FileContent{
		Path: s.Rel(abs), Size: st.Size(), Mtime: st.ModTime(),
		Editable: editableExt[ext] && st.Size() <= MaxEditBytes,
	}
	if !fc.Editable {
		fc.Binary = true
		return fc, nil
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取失败：%w", err)
	}
	if bytes.IndexByte(b, 0) >= 0 {
		fc.Binary, fc.Editable = true, false
		return fc, nil
	}
	fc.Content = string(b)
	return fc, nil
}

// Write 覆盖保存文本文件（不存在则连同父目录一起创建）。
func (s *Store) Write(rel, content string) (*FileContent, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	if err := s.checkWritable(abs, rel); err != nil {
		return nil, err
	}
	if len(content) > MaxEditBytes {
		return nil, fmt.Errorf("内容超过上限（%d 字节）", MaxEditBytes)
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		return nil, fmt.Errorf("目标是目录：%s", rel)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("创建目录失败：%w", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("写入失败：%w", err)
	}
	return s.Read(s.Rel(abs))
}

// Create 新建空文件或目录；已存在报错（不覆盖）。
func (s *Store) Create(rel string, isDir bool) (*Node, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err == nil {
		return nil, fmt.Errorf("已存在：%s", rel)
	}
	if isDir {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return nil, fmt.Errorf("创建目录失败：%w", err)
		}
		n := Node{Name: filepath.Base(abs), Path: s.Rel(abs), Type: "dir"}
		if info, err := os.Stat(abs); err == nil {
			n.Mtime = info.ModTime()
		}
		return &n, nil
	}
	if err := s.checkWritable(abs, rel); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("创建目录失败：%w", err)
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("创建失败：%w", err)
	}
	_ = f.Close()
	n := Node{Name: filepath.Base(abs), Path: s.Rel(abs), Type: "file"}
	if info, err := os.Stat(abs); err == nil {
		n.Mtime, n.Size = info.ModTime(), info.Size()
	}
	return &n, nil
}

// Delete 删除文件或**空**目录（非空目录拒绝：避免误删整棵子树）。
func (s *Store) Delete(rel string) error {
	abs, err := s.resolve(rel)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("文件不存在：%s", rel)
	}
	if st.IsDir() {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return fmt.Errorf("读取目录失败：%w", err)
		}
		if len(entries) > 0 {
			return fmt.Errorf("目录非空（%d 项），请先清空：%s", len(entries), rel)
		}
	}
	if err := os.Remove(abs); err != nil {
		return fmt.Errorf("删除失败：%w", err)
	}
	return nil
}

// Rename 重命名/移动（目标已存在则拒绝）。
func (s *Store) Rename(from, to string) (*Node, error) {
	absFrom, err := s.resolve(from)
	if err != nil {
		return nil, err
	}
	absTo, err := s.resolve(to)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absFrom); err != nil {
		return nil, fmt.Errorf("源不存在：%s", from)
	}
	if _, err := os.Stat(absTo); err == nil {
		return nil, fmt.Errorf("目标已存在：%s", to)
	}
	if err := s.checkWritable(absTo, to); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
		return nil, fmt.Errorf("创建目录失败：%w", err)
	}
	if err := os.Rename(absFrom, absTo); err != nil {
		return nil, fmt.Errorf("重命名失败：%w", err)
	}
	n := Node{Name: filepath.Base(absTo), Path: s.Rel(absTo), Type: "file"}
	if info, err := os.Stat(absTo); err == nil {
		n.Mtime, n.Size = info.ModTime(), info.Size()
		if info.IsDir() {
			n.Type = "dir"
		}
	}
	return &n, nil
}

// checkWritable 写入红线：扩展名必须在文本白名单内。
func (s *Store) checkWritable(abs, rel string) error {
	ext := strings.ToLower(filepath.Ext(abs))
	if !editableExt[ext] {
		if ext == "" {
			return fmt.Errorf("不允许写无扩展名文件：%s", rel)
		}
		return fmt.Errorf("不允许写该类型（%s）：%s", ext, rel)
	}
	return nil
}
