package syncer

import (
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// TestSourceScriptOpInvokesCallback 锁住"客户端装了什么源，
// 服务端就持有什么源"这条链路。
//
// 语义澄清（与用户确认过）：同步的粒度是**已安装的源**，
// 不是 venera-configs 的全量清单。后者会让每台设备凭空多出
// 几十个用户根本没启用的源，探索页/搜索页被噪声塞满。
func TestSourceScriptOpInvokesCallback(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	var got []model.SourceScriptOpPayload
	s.OnSourceScript = func(p model.SourceScriptOpPayload) error {
		got = append(got, p)
		return nil
	}

	_, err := s.Push(PushRequest{
		ClientID: "phone",
		Lamport:  1,
		Ops: []PushOp{
			{Type: string(model.OpSourceScript), Payload: model.SourceScriptOpPayload{
				Source:  "jm",
				Name:    "禁漫天堂",
				Version: "1.4.0",
				Script:  "class JM extends ComicSource { key = 'jm' }",
			}},
		},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 source script callback, got %d", len(got))
	}
	if got[0].Source != "jm" {
		t.Errorf("source = %q, want jm", got[0].Source)
	}
	if got[0].Script == "" {
		t.Error("script must not be empty")
	}
}

// TestSourceScriptOpRejectsEmptyScript 确认空脚本不会被当成有效同步。
func TestSourceScriptOpRejectsEmptyScript(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	var called bool
	s.OnSourceScript = func(model.SourceScriptOpPayload) error {
		called = true
		return nil
	}

	resp, err := s.Push(PushRequest{
		ClientID: "phone",
		Lamport:  1,
		Ops: []PushOp{
			{Type: string(model.OpSourceScript), Payload: model.SourceScriptOpPayload{
				Source: "jm", Script: "",
			}},
		},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if called {
		t.Error("empty script must not reach the install callback")
	}
	if resp.Applied != 0 {
		t.Errorf("applied = %d, want 0", resp.Applied)
	}
	if len(resp.Warnings) == 0 {
		t.Error("expected a warning for the rejected op")
	}
}

// TestSourceRemoveOpInvokesCallback 确认卸载源也会同步。
func TestSourceRemoveOpInvokesCallback(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	var removed []string
	s.OnSourceRemove = func(key string) error {
		removed = append(removed, key)
		return nil
	}

	_, err := s.Push(PushRequest{
		ClientID: "phone",
		Lamport:  1,
		Ops: []PushOp{
			{Type: string(model.OpSourceRemove), Payload: model.SourceRemoveOpPayload{
				Source: "wnacg",
			}},
		},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(removed) != 1 || removed[0] != "wnacg" {
		t.Fatalf("expected wnacg removed, got %v", removed)
	}
}

// TestSnapshotOmitsDeviceOnlySettings 锁住"只排除设备特有配置"的原则。
//
// 多设备是同一个人在用，所以代理/设备 ID 这类东西不该跟着走，
// 而 explore_pages / categories 这种使用习惯必须同步。
func TestSnapshotOmitsDeviceOnlySettings(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	// 写入一批设置：混合设备特有条目与应当同步的条目
	must := func(k string, v any) {
		if err := st.PutSetting(k, v); err != nil {
			t.Fatalf("put setting %s: %v", k, err)
		}
	}
	must("explore_pages", []any{"Picacg", "禁漫天堂"})
	must("categories", []any{"picacg"})
	must("searchSources", []any{"jm", "picacg"})
	must("readerMode", "page")
	must("proxy", "http://127.0.0.1:7890") // 设备特有 → 应被剔除
	must("deviceId", "abc123")
	must("nas", map[string]any{"baseUrl": "http://nas:18080"})

	snap, err := s.BuildSnapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// 应当存在的
	for _, k := range []string{"explore_pages", "categories", "searchSources", "readerMode"} {
		if _, ok := snap.Settings[k]; !ok {
			t.Errorf("setting %q must be synced but is missing", k)
		}
	}
	// 不应存在的
	for _, k := range DeviceOnlySettings {
		if _, ok := snap.Settings[k]; ok {
			t.Errorf("setting %q is device-only and must be omitted", k)
		}
	}
}

// TestSnapshotHistoryHasMillisTimestamp 锁住历史同步的时间字段。
//
// 客户端用 LWW 比较，读的是毫秒 int。服务端内嵌的 History 只序列化
// 出 `updatedAt`（RFC3339 字符串），早期实现没有补 `at`，
// 客户端拿到的 remoteTime 恒为 0，"本地 >= 0" 恒成立 → 所有历史被跳过。
// 这是"收藏同步了、历史一条都没有"的直接原因。
func TestSnapshotHistoryHasMillisTimestamp(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	_, err := s.Push(PushRequest{
		ClientID: "phone",
		Lamport:  1,
		Ops: []PushOp{
			{Type: string(model.OpHistoryUpdate), Payload: model.HistoryOpPayload{
				Source: "jm", ComicID: "42", Title: "T",
				SourceType: 123, Ep: 2, Page: 7,
				At: 1792937056567,
			}},
		},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	snap, err := s.BuildSnapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Histories) != 1 {
		t.Fatalf("expected 1 history, got %d", len(snap.Histories))
	}
	h := snap.Histories[0]
	if h.At <= 0 {
		t.Fatalf("history.at must be a positive millisecond timestamp, got %d", h.At)
	}
	// 与客户端上送的 at 应一致（同一毫秒）
	if h.At != 1792937056567 {
		t.Errorf("history.at = %d, want 1792937056567", h.At)
	}
}

var _ = store.SourceInfo{}
