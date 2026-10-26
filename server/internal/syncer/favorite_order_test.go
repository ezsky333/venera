package syncer

import (
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

func intPtr(v int) *int { return &v }

// TestFavoriteOrderIsPreserved 锁住「收藏顺序被颠倒」这个真实 bug。
//
// 客户端列表的排列完全由 display_order 决定，与 added_at 无关：
// 新收藏默认插到开头（minValue-1），所以**编号越小越靠前**。
//
// 早期实现没有同步这个字段，接收端每条都走"插到开头"的默认分支，
// 逐条应用时每一条都跑到最前面 —— 最终整批收藏的顺序被完全颠倒。
//
// 修复后的契约：order 原样落库，且按 order 升序读出。
func TestFavoriteOrderIsPreserved(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	// 模拟手机端：三条收藏，order 分别是 10 / 20 / 30（10 在最前面）
	ops := []PushOp{
		{Type: string(model.OpFavoriteAdd), Payload: model.FavoriteOpPayload{
			Folder: "Fav", Source: "jm", ComicID: "first", Title: "A",
			SourceType: 1, Order: intPtr(10),
		}},
		{Type: string(model.OpFavoriteAdd), Payload: model.FavoriteOpPayload{
			Folder: "Fav", Source: "jm", ComicID: "second", Title: "B",
			SourceType: 1, Order: intPtr(20),
		}},
		{Type: string(model.OpFavoriteAdd), Payload: model.FavoriteOpPayload{
			Folder: "Fav", Source: "jm", ComicID: "third", Title: "C",
			SourceType: 1, Order: intPtr(30),
		}},
	}
	if _, err := s.Push(PushRequest{ClientID: "phone", Lamport: 1, Ops: ops}); err != nil {
		t.Fatalf("push: %v", err)
	}

	list, err := st.ListFavorites("Fav", 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 favorites, got %d", len(list))
	}

	// 顺序必须是 first → second → third
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if list[i].ID != w {
			t.Errorf("position %d: got %q, want %q (order=%d)",
				i, list[i].ID, w, list[i].DisplayOrder)
		}
	}
}

// TestFavoriteOrderRoundTripsSnapshot 确认快照也带 order，
// 否则新设备拉取后依然无法还原顺序。
func TestFavoriteOrderRoundTripsSnapshot(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	_, err := s.Push(PushRequest{
		ClientID: "phone", Lamport: 1,
		Ops: []PushOp{
			{Type: string(model.OpFavoriteAdd), Payload: model.FavoriteOpPayload{
				Folder: "Fav", Source: "jm", ComicID: "x", Title: "X",
				SourceType: 1, Order: intPtr(-5),
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
	items := snap.Favorites["Fav"]
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].DisplayOrder != -5 {
		t.Errorf("displayOrder = %d, want -5", items[0].DisplayOrder)
	}
}

// TestFavoriteWithoutOrderStillWorks 确认老客户端（不传 order）不会报错。
//
// 兼容性要求：升级前推送的 op 里没有 order 字段，
// 这些记录应当以 order=0 落库，而不是被拒绝。
func TestFavoriteWithoutOrderStillWorks(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	_, err := s.Push(PushRequest{
		ClientID: "old-client", Lamport: 1,
		Ops: []PushOp{
			{Type: string(model.OpFavoriteAdd), Payload: model.FavoriteOpPayload{
				Folder: "Fav", Source: "jm", ComicID: "y", Title: "Y",
				SourceType: 1,
				// Order 故意留 nil
			}},
		},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	list, err := st.ListFavorites("Fav", 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}
	if list[0].ID != "y" {
		t.Errorf("got %q, want y", list[0].ID)
	}
}
