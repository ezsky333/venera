package store

import (
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

// TestDeferJobDoesNotConsumeAttempts 是这次修复的核心不变量：
// "推迟"和"失败"必须分开计数。
//
// 混在一起的后果（真机现场）：客户端刚连上、Cookie 还没同步，
// 200 个任务瞬间 attempts 打满被判 failed；等 Cookie 到了，
// 没有任何机制会重跑它们，用户看到的是"连上了但一本都下不动"。
func TestDeferJobDoesNotConsumeAttempts(t *testing.T) {
	st := newTestStore(t)

	id, err := st.CreateJob(&model.Job{
		Source: "picacg", ComicID: "abc", Title: "t", Origin: "favorite-sync",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := st.ClaimNextJob(); err != nil {
		t.Fatalf("claim: %v", err)
	}

	n, err := st.DeferJob(id, "waiting for client credentials")
	if err != nil {
		t.Fatalf("defer: %v", err)
	}
	if n != 1 {
		t.Fatalf("defer affected %d rows, want 1", n)
	}

	j, err := st.GetJob(id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.Status != model.JobPending {
		t.Fatalf("status = %s, want pending", j.Status)
	}
	if j.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0", j.Attempts)
	}
	if j.Error != "" {
		t.Fatalf("error = %q, want empty", j.Error)
	}

	// 再推迟几次也不能变成失败。
	for i := 0; i < 5; i++ {
		if _, err := st.ClaimNextJob(); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := st.DeferJob(id, "still waiting"); err != nil {
			t.Fatalf("defer: %v", err)
		}
	}
	j, _ = st.GetJob(id)
	if j.Status != model.JobPending || j.Attempts != 0 {
		t.Fatalf("after 5 deferrals: status=%s attempts=%d, want pending/0", j.Status, j.Attempts)
	}
}

// TestDeferJobOnlyTouchesRunning 确认推迟不会把已完成的任务拉回队列。
func TestDeferJobOnlyTouchesRunning(t *testing.T) {
	st := newTestStore(t)

	id, _ := st.CreateJob(&model.Job{Source: "jm", ComicID: "a", Title: "a"})
	if _, err := st.ClaimNextJob(); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.UpdateJob(id, model.JobDone, 1, "", ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	n, err := st.DeferJob(id, "late deferral")
	if err != nil {
		t.Fatalf("defer: %v", err)
	}
	if n != 0 {
		t.Fatalf("defer affected %d rows, want 0 (job is not running)", n)
	}
	j, _ := st.GetJob(id)
	if j.Status != model.JobDone {
		t.Fatalf("status = %s, want done", j.Status)
	}
}

// TestResetDeferredJobsOnlyWakesDeferred 确认唤醒不会误伤普通 pending 任务。
func TestResetDeferredJobsOnlyWakesDeferred(t *testing.T) {
	st := newTestStore(t)

	deferredID, _ := st.CreateJob(&model.Job{Source: "picacg", ComicID: "a", Title: "a"})
	plainID, _ := st.CreateJob(&model.Job{Source: "picacg", ComicID: "b", Title: "b"})

	// 只把第一条变成"推迟过"的状态。
	if _, err := st.ClaimNextJob(); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if n, err := st.DeferJob(deferredID, "waiting for client credentials"); err != nil || n != 1 {
		t.Fatalf("defer: n=%d err=%v", n, err)
	}

	n, err := st.ResetDeferredJobs("picacg")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset affected %d rows, want 1", n)
	}

	d, _ := st.GetJob(deferredID)
	if d.Status != model.JobPending || d.Attempts != 0 {
		t.Fatalf("deferred job: status=%s attempts=%d", d.Status, d.Attempts)
	}
	p, _ := st.GetJob(plainID)
	if p.Status != model.JobPending {
		t.Fatalf("plain job status = %s, want pending", p.Status)
	}
}

// TestSourceCredentialsKnown 固定"登录态是否已知"的判据：
// 收到过 source.data（哪怕是空 map）或 source.cookie 就算已知。
func TestSourceCredentialsKnown(t *testing.T) {
	st := newTestStore(t)

	if st.SourceCredentialsKnown("picacg") {
		t.Fatal("fresh store reports credentials known")
	}

	// 空 data 也算已知：客户端会为无凭证的源推一条空 data，
	// 语义是"这个源我交代过了，别再等"。
	if err := st.PutSourceData("picacg", map[string]any{}); err != nil {
		t.Fatalf("put source data: %v", err)
	}
	if !st.SourceCredentialsKnown("picacg") {
		t.Fatal("credentials unknown after empty source.data")
	}
	if st.SourceCredentialsKnown("jm") {
		t.Fatal("jm should still be unknown")
	}

	if err := st.PutCookies("jm", []model.CookieRecord{
		{Name: "sid", Value: "v", Domain: "jm.example"},
	}); err != nil {
		t.Fatalf("put cookies: %v", err)
	}
	if !st.SourceCredentialsKnown("jm") {
		t.Fatal("credentials unknown after cookies")
	}
}

// TestDeferredColumnMigration 确认老库升级后 deferred 列存在且默认 0。
func TestDeferredColumnMigration(t *testing.T) {
	st := newTestStore(t)
	id, _ := st.CreateJob(&model.Job{Source: "jm", ComicID: "a", Title: "a"})

	var deferred int
	if err := st.DB().QueryRow(`SELECT COALESCE(deferred,0) FROM jobs WHERE id = ?`, id).
		Scan(&deferred); err != nil {
		t.Fatalf("deferred column missing: %v", err)
	}
	if deferred != 0 {
		t.Fatalf("deferred = %d, want 0", deferred)
	}
}
