package source

import (
	"strings"
	"testing"
)

// TestInstallByKeyIsCaseInsensitive 锁住这个真实 bug：
//
//	komiic 的脚本里声明 `key = "Komiic"`（大写 K），而客户端同步上来
//	用的是规范化后的小写 `komiic`。严格相等比对会直接拒绝安装，日志是
//
//	  install source failed  source=komiic
//	  err="source key mismatch: declared \"Komiic\", expected \"komiic\""
//
// 用户看到的现象是"手机上有 9 个源，服务端只有 8 个，就它没有"。
func TestInstallByKeyIsCaseInsensitive(t *testing.T) {
	js := `
class _KomiicLike extends ComicSource {
    key = "Komiic"
    name = "komiic"
    version = "1.0.0"
}
`
	m := newInitTestManager(t, "_placeholder", "", &initTestStore{})

	if err := m.InstallByKey("komiic", js); err != nil {
		t.Fatalf("InstallByKey must accept case-differing key: %v", err)
	}

	// 存储键必须统一小写：否则按 key 查脚本会分裂成两个源
	if !m.Has("komiic") {
		t.Fatalf("script must be registered under lowercase key")
	}
	if m.Has("Komiic") {
		t.Fatalf("uppercase key must NOT become a second source")
	}
	if _, ok := m.Bundle("komiic"); !ok {
		t.Fatalf("bundle must be retrievable by lowercase key")
	}
}

// TestInstallByKeyRejectsTrulyDifferentKey 确认放宽大小写之后
// **没有**把"完全不同的 key"也放进来——那会让一台设备的脚本
// 覆盖掉另一个源。
func TestInstallByKeyRejectsTrulyDifferentKey(t *testing.T) {
	js := `
class _Other extends ComicSource {
    key = "something_else"
    name = "other"
    version = "1.0.0"
}
`
	m := newInitTestManager(t, "_placeholder", "", &initTestStore{})

	err := m.InstallByKey("komiic", js)
	if err == nil {
		t.Fatalf("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "source key mismatch") {
		t.Fatalf("want key mismatch error, got %v", err)
	}
	if m.Has("komiic") {
		t.Fatalf("rejected script must not be registered")
	}
}

// TestInstallByKeyPersistsDeclaredMeta 确认 install 之后元信息可用
// （名称/版本从脚本里解析，而不是沿用文件名）。
func TestInstallByKeyPersistsDeclaredMeta(t *testing.T) {
	js := `
class _Meta extends ComicSource {
    key = "_Meta"
    name = "元信息源"
    version = "2.3.4"
}
`
	m := newInitTestManager(t, "_placeholder", "", &initTestStore{})

	if err := m.InstallByKey("_meta", js); err != nil {
		t.Fatalf("InstallByKey: %v", err)
	}
	if got := m.Name("_meta"); got != "元信息源" {
		t.Fatalf("name = %q, want 元信息源", got)
	}
	b, ok := m.Bundle("_meta")
	if !ok {
		t.Fatalf("bundle missing")
	}
	if b.Version != "2.3.4" {
		t.Fatalf("version = %q, want 2.3.4", b.Version)
	}
}
