package jobs

import (
	"errors"
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// fakeRunner 是这些测试专用的 SourceRunner：按 comicID 决定报什么错。
type credRunner struct {
	err error
}

func (f *credRunner) LoadDetail(source, comicID string) (*model.ComicDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &model.ComicDetail{
		Comic: model.Comic{Source: source, ID: comicID, Title: "t"},
	}, nil
}

func (f *credRunner) LoadEpisodes(sourceKey, comicID, ep string) ([]string, error) {
	return []string{"p1"}, nil
}

func (f *credRunner) ImageConfigFor(sourceKey, imageURL, comicID, ep string) (*ImageConfig, error) {
	return &ImageConfig{URL: imageURL}, nil
}

func newCredStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir+"/data", dir+"/library")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newWorkerFor 构造一个带 downloader 的 worker。
//
// 必须注入 downloader：runJob 里 `w.dl == nil` 会直接判 failed
// ("no downloader configured")，走不到被测试的那段逻辑。
func newWorkerFor(t *testing.T, st *store.Store, runner SourceRunner) *Worker {
	t.Helper()
	w := NewWorker(st, runner, 1)
	w.SetDownloader(NewDownloader(st, runner, 1))
	return w
}

// claimOne 建一个任务并 claim 出来，模拟 worker 取到任务的那一刻。
func claimOne(t *testing.T, st *store.Store, source, comicID string) *model.Job {
	t.Helper()
	if _, err := st.CreateJob(&model.Job{
		Source: source, ComicID: comicID, Title: "t", Origin: "favorite-sync",
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	j, err := st.ClaimNextJob()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if j == nil {
		t.Fatal("claim returned nil job")
	}
	return j
}

// TestAuthErrorDeferredWhenCredentialsMissing 覆盖真机上最要命的那个场景：
//
//	客户端刚连上 NAS → 收藏先落库 → worker 立刻开跑 →
//	此时 source_cookies 还是空的 → picacg "Not logged in"。
//
// 旧行为：直接判 failed，等 Cookie 同步上来时任务已经死了，
// 用户看到"连上了但一本都下不动"（现场 42 个 failed / 0 个 cached）。
// 新行为：推迟（deferred+1，attempts 不动），凭证到达后唤醒。
func TestAuthErrorDeferredWhenCredentialsMissing(t *testing.T) {
	st := newCredStore(t)
	runner := &credRunner{err: errors.New("load comic info: Error: Not logged in\n\tat reLogin (<eval>:52:23(7))")}
	w := newWorkerFor(t, st, runner)

	job := claimOne(t, st, "picacg", "abc")
	w.runJob(0, job)

	got, err := st.GetJob(job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != model.JobPending {
		t.Fatalf("status = %s, want pending (deferred, not failed); error=%q", got.Status, got.Error)
	}
	if got.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0 (a deferral must not consume retries)", got.Attempts)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want empty (deferral is not a failure)", got.Error)
	}
}

// TestAuthErrorFailsWhenCredentialsAlreadyKnown 是对照组：
// 凭证早就同步过了还登不上 → 凭证真的失效，必须判失败，
// 否则任务会在队列里无限打转、把 worker 全占住。
func TestAuthErrorFailsWhenCredentialsAlreadyKnown(t *testing.T) {
	st := newCredStore(t)
	runner := &credRunner{err: errors.New("load comic info: Error: Not logged in")}
	w := newWorkerFor(t, st, runner)

	if err := st.PutCookies("picacg", []model.CookieRecord{
		{Name: "token", Value: "x", Domain: "picacg.example"},
	}); err != nil {
		t.Fatalf("put cookies: %v", err)
	}

	job := claimOne(t, st, "picacg", "abc")
	w.runJob(0, job)

	got, _ := st.GetJob(job.ID)
	if got.Status != model.JobFailed {
		t.Fatalf("status = %s, want failed (credentials were already synced)", got.Status)
	}
}

// TestExhentai302DeferredWhenCredentialsMissing 覆盖 ehentai 的表现形式：
// 未登录时源站 302 到登录页，报的是 "Invalid status code: 302"。
func TestExhentai302DeferredWhenCredentialsMissing(t *testing.T) {
	st := newCredStore(t)
	runner := &credRunner{err: errors.New("load comic info: Invalid status code: 302")}
	w := newWorkerFor(t, st, runner)

	job := claimOne(t, st, "ehentai", "https://exhentai.org/g/1/abc/")
	w.runJob(0, job)

	got, _ := st.GetJob(job.ID)
	if got.Status != model.JobPending {
		t.Fatalf("status = %s, want pending (deferred)", got.Status)
	}
}

// TestTransientErrorStillRetries 保证新分支没有吃掉原有重试逻辑。
func TestTransientErrorStillRetries(t *testing.T) {
	st := newCredStore(t)
	runner := &credRunner{err: errors.New("load comic info: read tcp: i/o timeout")}
	w := newWorkerFor(t, st, runner)

	job := claimOne(t, st, "jm", "abc")
	w.runJob(0, job)

	got, _ := st.GetJob(job.ID)
	if got.Status != model.JobPending {
		t.Fatalf("status = %s, want pending (retry)", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.Attempts)
	}
}

// TestIsAuthError 固定住各源实际报出来的登录类错误文案。
func TestIsAuthError(t *testing.T) {
	yes := []string{
		"load comic info: Error: Not logged in\n\tat reLogin (<eval>:52:23(7))",
		"load comic info: Invalid status code: 302",
		"load comic info: Invalid Status Code 302",
		"load comic info: invalid login",
		"load comic info: Invalid status code: 403",
	}
	for _, m := range yes {
		if !isAuthError(m) {
			t.Errorf("isAuthError(%q) = false, want true", m)
		}
	}
	no := []string{
		"load comic info: read tcp: i/o timeout",
		"load comic info: Invalid Data",
		"load comic info: EOF",
		"load comic info: invalid url: null/comics/1",
	}
	for _, m := range no {
		if isAuthError(m) {
			t.Errorf("isAuthError(%q) = true, want false", m)
		}
	}
}
