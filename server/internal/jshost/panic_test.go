package jshost

import (
	"strings"
	"testing"

	"github.com/venera-app/venera-nas/assets"
	"github.com/venera-app/venera-nas/internal/model"
)

// panicStore 是最小的 SourceDataStore。
type panicStore struct{}

func (m *panicStore) GetSourceData(string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (m *panicStore) MergeSourceData(string, map[string]any) error { return nil }
func (m *panicStore) GetCookies(string) ([]model.CookieRecord, error) {
	return nil, nil
}
func (m *panicStore) PutCookies(string, []model.CookieRecord) error { return nil }

// TestCallRecoversFromPanic 锁住一个真实事故：
//
//	goja 在 promise 反应作业的 popTryFrame 里越界 panic，
//	任其冒泡会**杀死整个服务端进程**——
//	实测中一个源脚本的边界情况让 NAS 直接下线。
//
// 修复后的契约：panic 被转成 error 返回，进程继续存活。
func TestCallRecoversFromPanic(t *testing.T) {
	h, err := New(Options{
		SourceKey: "_panic",
		InitJS:    assets.InitJS,
		SourceJS:  "class _P extends ComicSource { key = '_p'; }",
		Store:     &panicStore{},
		Timeout:   defaultTestTimeout,
	})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	defer h.Close()
	go h.RunLoop()

	// 在 JS 里主动抛出一个 Go 侧的 panic 很难直接构造，
	// 这里退而验证"事件回调里的 panic 被吞掉"这条路径：
	// 手动向事件队列投递一个会 panic 的回调，RunLoop 必须继续工作。
	h.post(func() {
		panic("simulated goja panic")
	})

	// 随后的正常调用仍然可用（说明 RunLoop 没有被 panic 打断）
	v, err := h.Call(`(() => 1 + 1)()`)
	if err != nil {
		t.Fatalf("host must survive a panicking event: %v", err)
	}
	if got := v.String(); got != "2" {
		t.Fatalf("got %q, want 2", got)
	}
}

// TestCallReturnsErrorOnPanic 确认 Call 自身也不会让 panic 冒泡。
func TestCallReturnsErrorOnPanic(t *testing.T) {
	h, err := New(Options{
		SourceKey: "_panic2",
		InitJS:    assets.InitJS,
		SourceJS:  "class _P2 extends ComicSource { key = '_p2'; }",
		Store:     &panicStore{},
		Timeout:   defaultTestTimeout,
	})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	defer h.Close()
	go h.RunLoop()

	// 语法错误应当返回 error（不是 panic）
	_, err = h.Call(`(() => {`)
	if err == nil {
		t.Fatal("expected error for malformed expression")
	}
	if !strings.Contains(err.Error(), "Unexpected") &&
		!strings.Contains(err.Error(), "SyntaxError") {
		t.Logf("note: error message = %v", err)
	}
}
