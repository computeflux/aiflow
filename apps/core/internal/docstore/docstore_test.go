package docstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStore 建临时 docs 目录 + 若干文件（plan/ 子目录、图片、大文件）。
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mustWrite("index.md", "# 索引\n")
	mustWrite("plan/S9.md", "# S9\n内容")
	mustWrite("ppt/zh/index.html", "<html><body>hi</body></html>")
	mustWrite("ppt/zh/images/logo.png", "\x89PNG\x00binary")
	return New(root), root
}

// TestPathEscape 锁定路径红线：任何越界/非法路径必须被拒（写/读/删/raw 全口径）。
func TestPathEscape(t *testing.T) {
	s, root := newTestStore(t)
	secret := filepath.Join(filepath.Dir(root), "secret.md")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	bad := []string{
		"../secret.md", "plan/../../secret.md", "/etc/passwd", "..", ".",
		"", "   ", "plan/./../..", "\\..\\secret.md", ".git/config", "node_modules/x.js",
	}
	for _, p := range bad {
		if _, err := s.resolve(p); err == nil {
			t.Errorf("resolve(%q) 应被拒绝但通过了", p)
		}
		if _, err := s.Read(p); err == nil {
			t.Errorf("Read(%q) 应被拒绝但通过了", p)
		}
		if _, err := s.Write(p, "x"); err == nil {
			t.Errorf("Write(%q) 应被拒绝但通过了", p)
		}
		if err := s.Delete(p); err == nil {
			t.Errorf("Delete(%q) 应被拒绝但通过了", p)
		}
		if _, err := s.ResolveForRaw(p); err == nil {
			t.Errorf("ResolveForRaw(%q) 应被拒绝但通过了", p)
		}
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "top secret" {
		t.Fatalf("根外文件被改动：%v %q", err, string(b))
	}
}

// TestSymlinkEscape 锁定软链逃逸：目录内指向根外的符号链接不可读写。
func TestSymlinkEscape(t *testing.T) {
	s, root := newTestStore(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(root, "link.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("符号链接不可用：%v", err)
	}
	if _, err := s.Read("link.md"); err == nil {
		t.Error("软链指向根外应被拒绝")
	}
}

// TestTreeAndCRUD 锁定目录树形态与增删改读。
func TestTreeAndCRUD(t *testing.T) {
	s, _ := newTestStore(t)
	tree, err := s.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if tree.Type != "dir" || tree.Path != "" {
		t.Errorf("根节点 = %+v，want dir/空 path", tree)
	}
	// 目录在前：plan/ppt 应排在 index.md 之前
	if len(tree.Children) == 0 || tree.Children[0].Type != "dir" {
		t.Fatalf("子节点顺序错误（目录应在前）：%+v", tree.Children)
	}
	names := map[string]string{}
	for _, c := range tree.Children {
		names[c.Name] = c.Type
	}
	if names["plan"] != "dir" || names["ppt"] != "dir" || names["index.md"] != "file" {
		t.Errorf("树缺少节点：%+v", names)
	}

	// 读文本
	fc, err := s.Read("plan/S9.md")
	if err != nil || !fc.Editable || fc.Content != "# S9\n内容" {
		t.Fatalf("Read plan/S9.md = %+v err=%v", fc, err)
	}
	// 读二进制（图片）：不返回内容、标 binary
	img, err := s.Read("ppt/zh/images/logo.png")
	if err != nil || !img.Binary || img.Content != "" {
		t.Fatalf("二进制文件应只读且不回内容：%+v err=%v", img, err)
	}
	// 写图片必须被拒（白名单）
	if _, err := s.Write("ppt/zh/images/logo.png", "x"); err == nil {
		t.Error("写二进制扩展名应被拒绝")
	}
	// 新建 → 写 → 读
	if _, err := s.Create("plan/new.md", false); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create("plan/new.md", false); err == nil {
		t.Error("重复新建应报错")
	}
	if _, err := s.Write("plan/new.md", "hello"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fc, _ := s.Read("plan/new.md"); fc.Content != "hello" {
		t.Errorf("写入后读回 = %q", fc.Content)
	}
	// 重命名
	if _, err := s.Rename("plan/new.md", "plan/renamed.md"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := s.Read("plan/new.md"); err == nil {
		t.Error("重命名后旧路径应不存在")
	}
	// 删除
	if err := s.Delete("plan/renamed.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 非空目录拒绝删除；空目录可删
	if err := s.Delete("plan"); err == nil {
		t.Error("非空目录删除应被拒绝")
	}
	if _, err := s.Create("plan/empty", true); err != nil {
		t.Fatalf("Create dir: %v", err)
	}
	if err := s.Delete("plan/empty"); err != nil {
		t.Errorf("空目录删除应成功：%v", err)
	}
}

// TestWriteCreatesParents 锁定「写不存在路径自动建父目录」（AI 生成新文档常用）。
func TestWriteCreatesParents(t *testing.T) {
	s, root := newTestStore(t)
	if _, err := s.Write("a/b/c.md", "x"); err != nil {
		t.Fatalf("Write 应自动建父目录：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b", "c.md")); err != nil {
		t.Errorf("文件未落地：%v", err)
	}
}

// TestSizeLimit 锁定超大文件不进编辑器（标 binary 只读）。
func TestSizeLimit(t *testing.T) {
	s, root := newTestStore(t)
	big := filepath.Join(root, "big.md")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", MaxEditBytes+1)), 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	fc, err := s.Read("big.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !fc.Binary || fc.Editable || fc.Content != "" {
		t.Errorf("超大文件应为 binary/只读/无内容：%+v", fc)
	}
	if _, err := s.Write("big.md", strings.Repeat("a", MaxEditBytes+1)); err == nil {
		t.Error("超限写入应被拒绝")
	}
}
