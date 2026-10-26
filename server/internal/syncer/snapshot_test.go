package syncer

import (
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

// TestSnapshotCarriesSourceTypeForEveryFavorite 锁住这个真实 bug：
//
//	新设备（平板/模拟器）刚装好 App 时**一个漫画源都没有**。
//	客户端应用快照时必须能在不依赖"源已安装"的情况下把收藏落地。
//	早期快照只给了 source/sourceType（可能为 0），客户端又用
//	`ComicSource.find(sourceKey) == null → return` 过滤，
//	结果服务端 993 条收藏在新设备上一条都看不到。
//
// 修复后的契约：
//  1. 每个收藏都带非零 sourceType；
//  2. 每个收藏都带 sourceKey（客户端不必从 ComicType 反查源对象）。
func TestSnapshotCarriesSourceTypeForEveryFavorite(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	// 先推两条收藏，模拟手机端上传
	_, err := s.Push(PushRequest{
		ClientID: "phone",
		Lamport:  1,
		Ops: []PushOp{
			{Type: string(model.OpFavoriteAdd), Payload: model.FavoriteOpPayload{
				Folder: "Fav", Source: "jm", ComicID: "1", Title: "A", SourceType: 12345,
			}},
			{Type: string(model.OpFavoriteAdd), Payload: model.FavoriteOpPayload{
				Folder: "Fav", Source: "picacg", ComicID: "2", Title: "B", SourceType: 6789,
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

	total := 0
	for folder, items := range snap.Favorites {
		for _, it := range items {
			total++
			if it.SourceType == 0 {
				t.Errorf("favorite %s/%s in folder %q has zero sourceType; "+
					"client cannot resolve it without installed sources",
					it.Source, it.ID, folder)
			}
			if it.SourceKey == "" {
				t.Errorf("favorite %s/%s is missing sourceKey", it.Source, it.ID)
			}
			if it.SourceKey != it.Source {
				t.Errorf("sourceKey %q != source %q", it.SourceKey, it.Source)
			}
		}
	}
	if total != 2 {
		t.Fatalf("expected 2 favorites in snapshot, got %d", total)
	}
}

// TestSnapshotHistoryCarriesSourceType 同样约束阅读进度。
func TestSnapshotHistoryCarriesSourceType(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	_, err := s.Push(PushRequest{
		ClientID: "phone",
		Lamport:  1,
		Ops: []PushOp{
			{Type: string(model.OpHistoryUpdate), Payload: model.HistoryOpPayload{
				Source: "jm", ComicID: "9", Title: "H",
				SourceType: 111, Ep: 3, Page: 5,
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
	if h.SourceType == 0 {
		t.Error("history has zero sourceType")
	}
	if h.SourceKey == "" {
		t.Error("history is missing sourceKey")
	}
}
