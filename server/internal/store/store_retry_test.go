package store

import (
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir+"/data", dir+"/library")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreateJob(t *testing.T, s *Store, source, comicID string) int64 {
	t.Helper()
	id, err := s.CreateJob(&model.Job{
		Source: source, ComicID: comicID, Title: comicID,
		Origin: "favorite-sync",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return id
}

func failJob(t *testing.T, s *Store, id int64) {
	t.Helper()
	if err := s.UpdateJob(id, model.JobFailed, 0, "", "boom"); err != nil {
		t.Fatalf("fail job %d: %v", id, err)
	}
}

// RetryFailedJobs 必须只复活每部漫画最新的一条失败记录。
// 否则多轮重试留下的历史失败会全部重跑，队列膨胀并对同一部漫画重复下载。
func TestRetryFailedJobsDedupesPerComic(t *testing.T) {
	s := newTestStore(t)

	// 同一部漫画留下 3 条失败历史（模拟多轮重试）
	old1 := mustCreateJob(t, s, "picacg", "A")
	failJob(t, s, old1)
	old2 := mustCreateJob(t, s, "picacg", "A")
	failJob(t, s, old2)
	newest := mustCreateJob(t, s, "picacg", "A")
	failJob(t, s, newest)

	// 另一部漫画，一条失败
	other := mustCreateJob(t, s, "jm", "B")
	failJob(t, s, other)

	// 一个已完成的任务，不该被复活
	done := mustCreateJob(t, s, "picacg", "C")
	if err := s.UpdateJob(done, model.JobDone, 1, "complete", ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.RetryFailedJobs("")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	// 期望：A 只有最新一条 + B 一条 = 2
	if n != 2 {
		t.Fatalf("requeued %d, want 2", n)
	}

	assertStatus := func(id int64, want model.JobStatus) {
		t.Helper()
		j, err := s.GetJob(id)
		if err != nil {
			t.Fatalf("get job %d: %v", id, err)
		}
		if j.Status != want {
			t.Errorf("job %d status = %s, want %s", id, j.Status, want)
		}
	}
	assertStatus(newest, model.JobPending)
	assertStatus(other, model.JobPending)
	assertStatus(old1, model.JobFailed)
	assertStatus(done, model.JobDone)
}

// 按源过滤：只重试指定源。
func TestRetryFailedJobsFiltersBySource(t *testing.T) {
	s := newTestStore(t)

	a := mustCreateJob(t, s, "jm", "A")
	failJob(t, s, a)
	b := mustCreateJob(t, s, "picacg", "B")
	failJob(t, s, b)

	n, err := s.RetryFailedJobs("jm")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n != 1 {
		t.Fatalf("requeued %d, want 1", n)
	}

	ja, _ := s.GetJob(a)
	jb, _ := s.GetJob(b)
	if ja.Status != model.JobPending {
		t.Errorf("jm job = %s, want pending", ja.Status)
	}
	if jb.Status != model.JobFailed {
		t.Errorf("picacg job = %s, want failed", jb.Status)
	}
}

// BumpJobAttempt 只在任务处于 running 时生效，并累加 attempts。
func TestBumpJobAttempt(t *testing.T) {
	s := newTestStore(t)
	id := mustCreateJob(t, s, "jm", "A")

	// pending 状态下不应被改（worker 还没领取）
	if n, _ := s.BumpJobAttempt(id, "x"); n != 0 {
		t.Fatalf("bump on pending = %d, want 0", n)
	}

	j, err := s.ClaimNextJob()
	if err != nil || j == nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.BumpJobAttempt(id, "timeout"); err != nil {
		t.Fatal(err)
	}

	j2, err := s.GetJob(id)
	if err != nil {
		t.Fatal(err)
	}
	if j2.Status != model.JobPending {
		t.Errorf("status = %s, want pending", j2.Status)
	}
	if j2.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", j2.Attempts)
	}
}
