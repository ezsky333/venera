package store

import (
	"errors"
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

func jobStatusOf(t *testing.T, s *Store, id int64) string {
	t.Helper()
	j, err := s.GetJob(id)
	if err != nil {
		t.Fatalf("get job %d: %v", id, err)
	}
	if j == nil {
		return ""
	}
	return string(j.Status)
}

// 终态任务可以删除。
func TestDeleteJobRemovesTerminalJob(t *testing.T) {
	s := newTestStore(t)
	id := mustCreateJob(t, s, "jm", "A")
	if err := s.UpdateJob(id, model.JobDone, 1, "complete", ""); err != nil {
		t.Fatal(err)
	}

	ok, err := s.DeleteJob(id)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !ok {
		t.Error("ok = false, want true")
	}
	if st := jobStatusOf(t, s, id); st != "" {
		t.Fatalf("job still present with status %q", st)
	}
}

// 运行中的任务必须拒绝删除。
//
// 理由：worker 只在章节间隙通过 GetJob 读 message 判断该不该停。
// 行被删掉后 GetJob 返回 nil，判断失效，worker 会继续把整本下完，
// 继续写 library、继续插 cached_chapters——只是任务记录没了。
func TestDeleteJobRefusesRunning(t *testing.T) {
	s := newTestStore(t)
	id := mustCreateJob(t, s, "jm", "A")
	if _, err := s.ClaimNextJob(); err != nil {
		t.Fatal(err)
	}
	if st := jobStatusOf(t, s, id); st != string(model.JobRunning) {
		t.Fatalf("status = %q, want running", st)
	}

	ok, err := s.DeleteJob(id)
	if !errors.Is(err, ErrJobRunning) {
		t.Fatalf("err = %v, want ErrJobRunning", err)
	}
	if ok {
		t.Error("ok = true, want false")
	}
	if st := jobStatusOf(t, s, id); st != string(model.JobRunning) {
		t.Fatalf("running job was removed, status = %q", st)
	}
}

// 不存在的任务返回 (false, nil)，不报错——重复点击删除不该弹错误。
func TestDeleteJobMissingIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	ok, err := s.DeleteJob(99999)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Error("ok = true, want false")
	}
}

// 暂停中的任务可以删除（用户主动停下的，不再需要）。
func TestDeleteJobAllowsPaused(t *testing.T) {
	s := newTestStore(t)
	id := mustCreateJob(t, s, "jm", "A")
	if _, err := s.PauseJob(id); err != nil {
		t.Fatal(err)
	}
	ok, err := s.DeleteJob(id)
	if err != nil || !ok {
		t.Fatalf("delete paused: ok=%v err=%v", ok, err)
	}
	if st := jobStatusOf(t, s, id); st != "" {
		t.Fatalf("job still present: %q", st)
	}
}

// 批量清理只删终态，pending/paused/running 必须保留。
func TestDeleteFinishedJobsKeepsActiveOnes(t *testing.T) {
	s := newTestStore(t)

	done := mustCreateJob(t, s, "jm", "done")
	if err := s.UpdateJob(done, model.JobDone, 1, "ok", ""); err != nil {
		t.Fatal(err)
	}
	failed := mustCreateJob(t, s, "jm", "failed")
	failJob(t, s, failed)
	cancelled := mustCreateJob(t, s, "jm", "cancelled")
	if _, err := s.CancelJob(cancelled); err != nil {
		t.Fatal(err)
	}
	pending := mustCreateJob(t, s, "jm", "pending")
	paused := mustCreateJob(t, s, "jm", "paused")
	if _, err := s.PauseJob(paused); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteFinishedJobs(nil)
	if err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if n != 3 {
		t.Fatalf("removed %d, want 3", n)
	}

	for _, id := range []int64{done, failed, cancelled} {
		if st := jobStatusOf(t, s, id); st != "" {
			t.Errorf("job %d should be gone, status = %q", id, st)
		}
	}
	if st := jobStatusOf(t, s, pending); st != string(model.JobPending) {
		t.Errorf("pending job status = %q, want pending", st)
	}
	if st := jobStatusOf(t, s, paused); st != string(model.JobPaused) {
		t.Errorf("paused job status = %q, want paused", st)
	}
}

// 显式指定状态时只清指定的那些。
func TestDeleteFinishedJobsWithStatusFilter(t *testing.T) {
	s := newTestStore(t)

	done := mustCreateJob(t, s, "jm", "done")
	if err := s.UpdateJob(done, model.JobDone, 1, "ok", ""); err != nil {
		t.Fatal(err)
	}
	failed := mustCreateJob(t, s, "jm", "failed")
	failJob(t, s, failed)

	n, err := s.DeleteFinishedJobs([]string{"failed"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if st := jobStatusOf(t, s, failed); st != "" {
		t.Errorf("failed job should be gone, status = %q", st)
	}
	if st := jobStatusOf(t, s, done); st != string(model.JobDone) {
		t.Errorf("done job status = %q, want done (not requested for deletion)", st)
	}
}

// 非法状态名被忽略，绝不能把 pending 队列清空。
func TestDeleteFinishedJobsIgnoresIllegalStatuses(t *testing.T) {
	s := newTestStore(t)
	pending := mustCreateJob(t, s, "jm", "A")

	n, err := s.DeleteFinishedJobs([]string{"pending", "running", "bogus"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("removed %d, want 0", n)
	}
	if st := jobStatusOf(t, s, pending); st != string(model.JobPending) {
		t.Fatalf("pending job was deleted, status = %q", st)
	}
}
