package syncer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir+"/data", dir+"/library")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestFavoriteAddIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	payload := model.FavoriteOpPayload{
		Folder: "默认", Source: "jm", ComicID: "42", Title: "T",
	}
	req := PushRequest{
		ClientID: "c1",
		Ops:      []PushOp{{Type: string(model.OpFavoriteAdd), Payload: payload}},
	}
	if _, err := s.Push(req); err != nil {
		t.Fatal(err)
	}
	// 重复推送同一条（不同设备可能重复上传）
	resp, err := s.Push(req)
	if err != nil {
		t.Fatal(err)
	}
	// 第二次不应改变状态，但操作会进入日志（用于广播）
	favs, err := st.ListFavorites("", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(favs) != 1 {
		t.Fatalf("expected 1 favorite, got %d", len(favs))
	}
	if resp.Applied > 1 {
		t.Fatalf("unexpected applied count %d", resp.Applied)
	}
}

func TestHistoryLWW(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	newer := time.Now().UnixMilli()
	older := newer - 60_000

	// 先推新，再推旧：旧的时间戳必须被忽略
	if _, err := s.Push(PushRequest{ClientID: "a", Ops: []PushOp{{
		Type: string(model.OpHistoryUpdate),
		Payload: model.HistoryOpPayload{
			Source: "jm", ComicID: "1", Ep: 10, Page: 5, At: newer,
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(PushRequest{ClientID: "b", Ops: []PushOp{{
		Type: string(model.OpHistoryUpdate),
		Payload: model.HistoryOpPayload{
			Source: "jm", ComicID: "1", Ep: 2, Page: 1, At: older,
		},
	}}}); err != nil {
		t.Fatal(err)
	}

	h, err := st.GetHistory("jm", "1")
	if err != nil {
		t.Fatal(err)
	}
	if h == nil {
		t.Fatal("history missing")
	}
	if h.Ep != 10 || h.Page != 5 {
		t.Fatalf("LWW failed: got ep=%d page=%d, want ep=10 page=5", h.Ep, h.Page)
	}
}

func TestEchoSuppression(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	if _, err := s.Push(PushRequest{ClientID: "phone-a", Ops: []PushOp{{
		Type:    string(model.OpFavoriteAdd),
		Payload: model.FavoriteOpPayload{Folder: "f", Source: "jm", ComicID: "1"},
	}}}); err != nil {
		t.Fatal(err)
	}

	// 自己拉自己：应为空
	resp, err := s.Pull(PullRequest{ClientID: "phone-a", Cursor: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Ops) != 0 {
		t.Fatalf("echo suppression failed: got %d ops", len(resp.Ops))
	}

	// 其他设备拉到 1 条
	resp2, err := s.Pull(PullRequest{ClientID: "phone-b", Cursor: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp2.Ops) != 1 {
		t.Fatalf("expected 1 op for other client, got %d", len(resp2.Ops))
	}
}

func TestPullCursorPagination(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	// 推 5 条
	var ops []PushOp
	for i := 0; i < 5; i++ {
		ops = append(ops, PushOp{
			Type: string(model.OpFavoriteAdd),
			Payload: model.FavoriteOpPayload{
				Folder: "f", Source: "jm", ComicID: string(rune('a' + i)),
			},
		})
	}
	if _, err := s.Push(PushRequest{ClientID: "c1", Ops: ops}); err != nil {
		t.Fatal(err)
	}

	// 分页拉取：每次 2 条
	cursor := int64(0)
	total := 0
	for i := 0; i < 10; i++ {
		resp, err := s.Pull(PullRequest{ClientID: "consumer", Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		total += len(resp.Ops)
		cursor = resp.Cursor
		if !resp.HasMore {
			break
		}
	}
	if total != 5 {
		t.Fatalf("pagination lost ops: got %d, want 5", total)
	}
}

func TestFavoriteRemove(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	if _, err := s.Push(PushRequest{ClientID: "c", Ops: []PushOp{{
		Type:    string(model.OpFavoriteAdd),
		Payload: model.FavoriteOpPayload{Folder: "f", Source: "jm", ComicID: "1"},
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(PushRequest{ClientID: "c", Ops: []PushOp{{
		Type:    string(model.OpFavoriteRemove),
		Payload: model.FavoriteOpPayload{Folder: "f", Source: "jm", ComicID: "1"},
	}}}); err != nil {
		t.Fatal(err)
	}
	favs, err := st.ListFavorites("f", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(favs) != 0 {
		t.Fatalf("favorite not removed: %d remain", len(favs))
	}
}

func TestSourceTypeReported(t *testing.T) {
	st := newTestStore(t)
	// 客户端上报权威值
	if err := st.PutSourceType("jm", 769844263); err != nil {
		t.Fatal(err)
	}
	// 读取时应返回上报值（而不是现场计算）
	got := st.SourceType("jm", 999999)
	if got != 769844263 {
		t.Fatalf("reported source type ignored: got %d", got)
	}
	// 未上报的走计算值
	got2 := st.SourceType("brand_new_source", 12345)
	if got2 != 12345 {
		t.Fatalf("computed fallback failed: got %d", got2)
	}
}

func TestSnapshot(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	if _, err := s.Push(PushRequest{ClientID: "c", Ops: []PushOp{
		{Type: string(model.OpFavoriteAdd), Payload: model.FavoriteOpPayload{
			Folder: "收藏", Source: "jm", ComicID: "1", Title: "A"}},
		{Type: string(model.OpHistoryUpdate), Payload: model.HistoryOpPayload{
			Source: "jm", ComicID: "1", Ep: 5, Page: 2, At: time.Now().UnixMilli()}},
	}}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.BuildSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Favorites["收藏"]) != 1 {
		t.Fatalf("snapshot favorites wrong: %+v", snap.Favorites)
	}
	if len(snap.Histories) != 1 {
		t.Fatalf("snapshot history wrong: %+v", snap.Histories)
	}
	if snap.Cursor == 0 {
		t.Fatal("snapshot cursor should be > 0")
	}
}

func TestBadPayloadDoesNotAbortBatch(t *testing.T) {
	st := newTestStore(t)
	s := New(st)

	// 第一个是坏操作，第二个是好的：整批不应中断
	resp, err := s.Push(PushRequest{ClientID: "c", Ops: []PushOp{
		{Type: "unknown.op", Payload: map[string]any{}},
		{Type: string(model.OpFavoriteAdd), Payload: model.FavoriteOpPayload{
			Folder: "f", Source: "jm", ComicID: "1"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Applied != 1 {
		t.Fatalf("expected 1 applied, got %d", resp.Applied)
	}
	if len(resp.Warnings) == 0 {
		t.Fatal("expected a warning for the bad op")
	}
	favs, _ := st.ListFavorites("", 10, 0)
	if len(favs) != 1 {
		t.Fatalf("good op was not applied")
	}
}

func TestOpPayloadRoundTrip(t *testing.T) {
	// 客户端传 json.RawMessage 与 struct 都应被正确处理
	st := newTestStore(t)
	s := New(st)

	raw, _ := json.Marshal(model.FavoriteOpPayload{
		Folder: "f", Source: "ehentai", ComicID: "7", Title: "X",
	})
	if _, err := s.Push(PushRequest{ClientID: "c", Ops: []PushOp{
		{Type: string(model.OpFavoriteAdd), Payload: json.RawMessage(raw)},
	}}); err != nil {
		t.Fatal(err)
	}
	favs, _ := st.ListFavorites("f", 10, 0)
	if len(favs) != 1 || favs[0].ID != "7" {
		t.Fatalf("raw payload failed: %+v", favs)
	}
}
