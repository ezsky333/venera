package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 目录存在但不可写时，Open 必须给出**看得懂**的错误。
//
// 真实事故（Docker 部署）：镜像里 `chown venera /data` 之后用
// `-v ./data:/data` 把宿主机目录盖上去，宿主机目录属于 root，
// 而容器以 uid 1000 运行。MkdirAll 对已存在目录直接返回成功，
// 于是错误推迟到 SQLite 才炸：
//
//	open store: migrate "CREATE TABLE IF NOT EXISTS sources (": unable to open database file (14)
//
// 这条消息里没有任何线索指向"权限"。现在应在 Open 阶段就报出来。
func TestOpenReportsUnwritableDir(t *testing.T) {
	base := t.TempDir()

	// 用一个**普通文件**占住 dataDir 这个路径：
	//   - Linux: MkdirAll 报 ENOTDIR
	//   - Windows: MkdirAll 也报错
	// 这比 chmod 0o500 可靠 —— Windows 的 ACL 不认 0500，
	// 目录照样可写，测试会假通过。
	blocked := filepath.Join(base, "data")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	_, err := Open(blocked, filepath.Join(base, "lib"))
	if err == nil {
		t.Fatal("want error when dataDir is a regular file, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, blocked) {
		t.Errorf("error should name the offending path, got: %s", msg)
	}
}

// 目录建不出来或写不进去时，错误必须点明是哪个目录。
//
// 两种失败路径都要覆盖：
//   - MkdirAll 直接失败（路径被文件占用 / 父目录不可建）
//   - 目录存在但 checkWritable 写不进去（Docker 挂载权限场景）
func TestUnwritableHintMentionsChown(t *testing.T) {
	base := t.TempDir()
	blocked := filepath.Join(base, "lib")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	_, err := Open(filepath.Join(base, "data"), blocked)
	if err == nil {
		t.Fatal("want error for blocked library dir, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "library dir") {
		t.Errorf("error should name the failing dir, got: %s", msg)
	}
	// 目录名必须出现在消息里，否则用户不知道该 chown 哪里
	if !strings.Contains(msg, blocked) {
		t.Errorf("error should contain the path %q, got: %s", blocked, err)
	}
}

// 正常路径下 Open 仍然能建表（回归保护）。
func TestOpenCreatesTables(t *testing.T) {
	base := t.TempDir()
	st, err := Open(filepath.Join(base, "data"), filepath.Join(base, "lib"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// 建表成功的标志：能查到 sources 表
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n); err != nil {
		t.Fatalf("sources table missing: %v", err)
	}
}

// checkWritable 对可写目录返回 nil，对不存在的路径返回错误。
func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	if err := checkWritable(dir); err != nil {
		t.Errorf("writable dir should pass: %v", err)
	}

	missing := filepath.Join(dir, "nope", "deeper")
	if err := checkWritable(missing); err == nil {
		t.Error("missing dir should fail write check")
	}
}
