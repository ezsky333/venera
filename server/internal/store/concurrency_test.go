package store_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// TestClaimNextJobSurvivesConcurrentWriter 复现真机上的 SQLITE_BUSY 刷屏。
//
// 现场日志（NAS，Docker，ZFS 机械盘）：
//
//	worker 0: claim job: database is locked (5) (SQLITE_BUSY)
//	worker 1: claim job: database is locked (5) (SQLITE_BUSY)
//	... 每 2 秒一轮，持续了整整数分钟
//
// 成因不是"没设 busy_timeout"，而是 database/sql 默认的 deferred BEGIN：
// ClaimNextJob 先 SELECT（拿读快照）再 UPDATE（升级成写）。WAL 下若这期间
// 有别的连接提交过，升级直接返回 SQLITE_BUSY_SNAPSHOT，而 busy_timeout
// 对 SQLITE_BUSY_SNAPSHOT **不生效**——立刻失败，永远等不满超时。
//
// 所以这个测试不看"有没有报错"，而是看"有没有等待"：
// 修复前是瞬时失败，修复后应该排队并成功。
func TestClaimNextJobSurvivesConcurrentWriter(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir+"/data", dir+"/library")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// 放几个待执行任务，让 claim 有东西可取。
	for i := 0; i < 8; i++ {
		if _, err := st.CreateJob(&model.Job{
			Source:   "jm",
			ComicID:  string(rune('a' + i)),
			Title:    "t",
			Chapters: nil,
			Origin:   "test",
		}); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}

	var (
		mu     sync.Mutex
		busy   []string
		other  []string
		claims int
		wg     sync.WaitGroup
	)

	stop := make(chan struct{})

	// 后台持续写：模拟"整批推送"期间一直有写事务在跑。
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			f := model.Favorite{
				Comic:    model.Comic{Source: "jm", ID: "w" + string(rune('a'+i%26))},
				Folder:   "Favorites",
				AddedAt:  time.Now(),
			}
			f.Title = "writer"
			if _, err := st.AddFavorite(f, time.Now().UnixMilli()); err != nil {
				mu.Lock()
				other = append(other, "writer: "+err.Error())
				mu.Unlock()
			}
			i++
		}
	}()

	// 4 个并发 claimer，对应 4 个 worker。
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for k := 0; k < 40; k++ {
				j, err := st.ClaimNextJob()
				mu.Lock()
				if err != nil {
					msg := err.Error()
					if strings.Contains(msg, "locked") || strings.Contains(msg, "SQLITE_BUSY") {
						busy = append(busy, msg)
					} else {
						other = append(other, msg)
					}
				} else if j != nil {
					claims++
				}
				mu.Unlock()
				time.Sleep(2 * time.Millisecond)
			}
		}(w)
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other[:min(3, len(other))])
	}
	if len(busy) > 0 {
		t.Fatalf("got %d SQLITE_BUSY errors under concurrency, want 0 (first: %s)",
			len(busy), busy[0])
	}
	if claims == 0 {
		t.Fatal("no job was ever claimed; test did not exercise the path")
	}
	t.Logf("claims=%d busy=0", claims)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
