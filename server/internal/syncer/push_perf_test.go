package syncer_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/venera-app/venera-nas/internal/store"
	"github.com/venera-app/venera-nas/internal/syncer"
)

// TestPushBatchTiming 量化"一次 200 条 fav.add 的推送要多久"。
//
// 现场证据：真机 NAS 上一批 200 条推送耗时 63s / 131s，
// 而客户端 dio receiveTimeout 只有 30s → 每一批都超时 →
// 后续批次（历史、源 Cookie）从未发出。
// 这个测试就是那条因果链的度量入口。
func TestPushBatchTiming(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir+"/data", dir+"/library")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sy := syncer.New(st)

	const n = 200
	ops := make([]syncer.PushOp, 0, n)
	for i := 0; i < n; i++ {
		ops = append(ops, syncer.PushOp{
			Type: "fav.add",
			Payload: map[string]any{
				"folder":  "Favorites",
				"source":  "jm",
				"comicId": fmt.Sprintf("comic-%d", i),
				"title":   fmt.Sprintf("Comic %d", i),
				"cover":   "",
				"tags":    []string{"a", "b"},
				"order":   i,
			},
		})
	}

	start := time.Now()
	resp, err := sy.Push(syncer.PushRequest{
		ClientID:   "dev-perf",
		DeviceName: "test",
		Lamport:    time.Now().UnixMilli(),
		Ops:        ops,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	perOp := elapsed / n
	t.Logf("push %d fav.add ops: total=%v applied=%d per-op=%v", n, elapsed, resp.Applied, perOp)
	if resp.Applied != n {
		t.Fatalf("applied = %d, want %d", resp.Applied, n)
	}

	// 客户端 receiveTimeout 是 30s；这里留足余量：
	// 一批 200 条必须在 5s 内完成，否则真机上必然再次超时。
	if elapsed > 5*time.Second {
		t.Fatalf("push of %d ops took %v, exceeds 5s budget", n, elapsed)
	}
}
