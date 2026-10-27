package syncer

import (
	"strings"
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

// 端到端验证墓碑在同步协议层的行为：
//
//	设备 A 离线 → B 删除该收藏 → A 恢复后推送队列里的旧 fav.add
//
// 期望：
//  1. 服务端不复活该收藏
//  2. 不把这条 add 写进 oplog（否则会广播给所有设备，等于全网复活）
//  3. 返回带 comicId 的 warning，让 A 自己删掉本地那条幽灵收藏
func TestStaleFavoriteAddIsRejectedAndWarned(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	payload := model.FavoriteOpPayload{
		Folder: "夹", Source: "jm", ComicID: "777", Title: "T",
	}

	// B 先收藏，再删除（lamport 用较大的值表示"更晚"）
	if _, err := s.Push(PushRequest{
		ClientID: "B", Lamport: 1000,
		Ops: []PushOp{{Type: string(model.OpFavoriteAdd), Payload: payload}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(PushRequest{
		ClientID: "B", Lamport: 2000,
		Ops: []PushOp{{Type: string(model.OpFavoriteRemove), Payload: payload}},
	}); err != nil {
		t.Fatal(err)
	}

	favs, err := st.ListFavorites("", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(favs) != 0 {
		t.Fatalf("favorite not removed, got %d", len(favs))
	}

	cursorBefore, err := st.MaxOpID()
	if err != nil {
		t.Fatal(err)
	}

	// A 恢复后推送队列里的旧 add（lamport 1000 < 墓碑 2000）
	resp, err := s.Push(PushRequest{
		ClientID: "A", Lamport: 1000,
		Ops: []PushOp{{Type: string(model.OpFavoriteAdd), Payload: payload}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1) 没有复活
	favs, err = st.ListFavorites("", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(favs) != 0 {
		t.Fatalf("stale add resurrected the favorite (%d rows)", len(favs))
	}

	// 2) 没进 oplog
	cursorAfter, err := st.MaxOpID()
	if err != nil {
		t.Fatal(err)
	}
	if cursorAfter != cursorBefore {
		t.Errorf("oplog grew from %d to %d; a rejected add must not be broadcast",
			cursorBefore, cursorAfter)
	}
	if resp.Applied != 0 {
		t.Errorf("applied = %d, want 0", resp.Applied)
	}

	// 3) 有 warning 且带得上 comicId（客户端要靠它定位本地那条）
	found := false
	for _, w := range resp.Warnings {
		if strings.Contains(w, "resurrect") && strings.Contains(w, "777") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no resurrect warning mentioning 777, got %v", resp.Warnings)
	}
}

// 用户删完之后**重新收藏**（lamport 更新）必须正常生效——
// 墓碑只能挡住旧消息，不能把用户的新动作也挡了。
func TestRefavoriteAfterDeleteSucceeds(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	payload := model.FavoriteOpPayload{
		Folder: "夹", Source: "jm", ComicID: "888", Title: "T",
	}
	if _, err := s.Push(PushRequest{
		ClientID: "A", Lamport: 1000,
		Ops: []PushOp{{Type: string(model.OpFavoriteAdd), Payload: payload}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(PushRequest{
		ClientID: "A", Lamport: 2000,
		Ops: []PushOp{{Type: string(model.OpFavoriteRemove), Payload: payload}},
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := s.Push(PushRequest{
		ClientID: "A", Lamport: 3000,
		Ops: []PushOp{{Type: string(model.OpFavoriteAdd), Payload: payload}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Applied != 1 {
		t.Fatalf("applied = %d, want 1; warnings=%v", resp.Applied, resp.Warnings)
	}
	favs, err := st.ListFavorites("", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(favs) != 1 {
		t.Fatalf("re-favorite not stored, got %d", len(favs))
	}
}
