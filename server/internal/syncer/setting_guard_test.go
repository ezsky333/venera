package syncer

import (
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

// TestSettingRejectsEmptyOverwrite 锁住这个真实事故：
//
//	pad 首次连接时本地 explore_pages / categories 都是空数组，
//	_bootstrapPush 把它们当"我的设置"推上来，
//	服务端原有的 ["Picacg","禁漫天堂"] 被清空，
//	手机下次同步看到空列表，探索页也空了。
//
// 修复后的契约：空值不能覆盖非空值。
func TestSettingRejectsEmptyOverwrite(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	// 手机先推一个非空列表
	if _, err := s.Push(PushRequest{
		ClientID: "phone",
		Lamport:  1,
		Ops: []PushOp{
			{Type: string(model.OpSettingUpdate), Payload: model.SettingOpPayload{
				Key:   "explore_pages",
				Value: []any{"Picacg", "禁漫天堂"},
			}},
		},
	}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// pad 推送空列表（模拟"我没配过"）
	if _, err := s.Push(PushRequest{
		ClientID: "pad",
		Lamport:  2,
		Ops: []PushOp{
			{Type: string(model.OpSettingUpdate), Payload: model.SettingOpPayload{
				Key:   "explore_pages",
				Value: []any{},
			}},
		},
	}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// 服务端应保留手机的值
	var got []any
	ok, err := st.GetSetting("explore_pages", &got)
	if err != nil {
		t.Fatalf("get setting: %v", err)
	}
	if !ok {
		t.Fatal("explore_pages disappeared")
	}
	if len(got) != 2 {
		t.Fatalf("empty value must not overwrite non-empty; got %v", got)
	}
}

// TestSettingAllowsNonEmptyOverwrite 确认正常的更新仍然生效。
func TestSettingAllowsNonEmptyOverwrite(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	if _, err := s.Push(PushRequest{
		ClientID: "phone", Lamport: 1,
		Ops: []PushOp{
			{Type: string(model.OpSettingUpdate), Payload: model.SettingOpPayload{
				Key: "explore_pages", Value: []any{"A"},
			}},
		},
	}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// 另一台设备显式改成另一组值（非空）→ 应当覆盖
	if _, err := s.Push(PushRequest{
		ClientID: "pad", Lamport: 2,
		Ops: []PushOp{
			{Type: string(model.OpSettingUpdate), Payload: model.SettingOpPayload{
				Key: "explore_pages", Value: []any{"A", "B"},
			}},
		},
	}); err != nil {
		t.Fatalf("push: %v", err)
	}

	var got []any
	if _, err := st.GetSetting("explore_pages", &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("non-empty overwrite must apply, got %v", got)
	}
}

// TestSettingFalseAndZeroAreNotEmpty 确认布尔/数字零不被当成"空"。
//
// reverseChapterOrder=false 是"不要倒序"的明确选择，
// 若把它判成空，用户就永远没法关掉倒序。
func TestSettingFalseAndZeroAreNotEmpty(t *testing.T) {
	if isEmptySettingValue(false) {
		t.Error("false must not be treated as empty")
	}
	if isEmptySettingValue(0) {
		t.Error("0 must not be treated as empty")
	}
	if isEmptySettingValue(float64(0)) {
		t.Error("0.0 must not be treated as empty")
	}
	if !isEmptySettingValue([]any{}) {
		t.Error("empty slice must be treated as empty")
	}
	if !isEmptySettingValue("") {
		t.Error("empty string must be treated as empty")
	}
	if !isEmptySettingValue(map[string]any{}) {
		t.Error("empty map must be treated as empty")
	}
}
