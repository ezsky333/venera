package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

// 端到端复现用户报告的问题，并验证修复。
//
// 线上现象（ehentai 4273534「用催眠将冰之女皇染上白浊」）：
//
//	cached_chapters.page_count = 64
//	磁盘上确实有 64 个文件，但编号是 0,1,11,12,14,… —— 中间有空洞
//	客户端请求 .../10.jpg 返回 404
//
// 根因链：
//  1. 图片按**原始索引**命名（idx），失败的图留下编号空洞；
//  2. page_count 记成"成功张数"，与文件编号最大值不一致；
//  3. 客户端固定用 0..page_count-1 拼 URL → 空洞处 404、结尾少页；
//  4. 这一章还被标记为已缓存 → 增量下载永远不再尝试。
//
// 修复后：缺页即失败、不落库、清掉半成品；成功时编号严格连续。
func TestNoPageHolesSurviveToDisk(t *testing.T) {
	st := newTestStore(t)

	const total = 20
	failAt := map[int]bool{3: true, 7: true, 11: true}
	pages := make([]string, total)
	for i := range pages {
		pages[i] = fmt.Sprintf("https://example.test/%d.jpg", i)
	}

	dl := NewDownloader(st, &fakeRunner{pages: pages}, 4)
	dl.SetFetch(func(url string, _ map[string]string) ([]byte, error) {
		for i := range failAt {
			if url == fmt.Sprintf("https://example.test/%d.jpg", i) {
				return nil, fmt.Errorf("status 404")
			}
		}
		return pngBytes(), nil
	})

	detail := &model.ComicDetail{
		Comic:    model.Comic{Source: "ehentai", ID: "4273534", Title: "t"},
		Chapters: []model.Chapter{{ID: "4273534", Title: "ch"}},
	}
	libDir := st.LibraryDir()

	err := dl.downloadChapter(detail, detail.Chapters[0], libDir)
	if err == nil {
		t.Fatal("chapter with 3 failed pages must fail, got nil (old bug: marked success)")
	}

	// 关键断言 1：不能入库（否则增量下载永不重试）
	ids, _ := st.CachedChapterIDs("ehentai", "4273534")
	if ids["4273534"] {
		t.Error("chapter with holes must not be recorded as cached")
	}

	// 关键断言 2：半成品目录必须清掉，不能留着误导下一次
	dir := filepath.Join(libDir, chapterDirName(detail.Chapters[0]))
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Errorf("partial dir should be removed, found %d entries", len(entries))
	}
}

// 成功路径：编号必须 0..n-1 连续，且 page_count == 文件数。
//
// 这是客户端不 404 的充要条件（api.handleChapter 用 page_count 拼 URL）。
func TestSuccessPathHasNoHoles(t *testing.T) {
	st := newTestStore(t)
	const total = 12
	pages := make([]string, total)
	for i := range pages {
		pages[i] = fmt.Sprintf("https://example.test/%d.jpg", i)
	}

	dl := NewDownloader(st, &fakeRunner{pages: pages}, 3)
	dl.SetFetch(func(string, map[string]string) ([]byte, error) {
		return pngBytes(), nil
	})

	detail := &model.ComicDetail{
		Comic:    model.Comic{Source: "s", ID: "ok", Title: "t"},
		Chapters: []model.Chapter{{ID: "1", Title: "ch"}},
	}
	if err := dl.downloadChapter(detail, detail.Chapters[0], st.LibraryDir()); err != nil {
		t.Fatalf("downloadChapter: %v", err)
	}

	ch, err := st.GetCachedChapter("s", "ok", "1")
	if err != nil || ch == nil {
		t.Fatalf("chapter missing: %v %v", ch, err)
	}
	if ch.PageCount != total {
		t.Fatalf("want page_count=%d, got %d", total, ch.PageCount)
	}

	dir := filepath.Join(st.LibraryDir(), filepath.FromSlash(ch.Dir))
	for i := 0; i < total; i++ {
		m, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%d.*", i)))
		if len(m) == 0 {
			t.Errorf("page %d missing → client would 404", i)
		}
	}
	// 不应该存在 >= total 的编号
	extra, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%d.*", total)))
	if len(extra) > 0 {
		t.Errorf("unexpected page index %d (page_count is %d)", total, ch.PageCount)
	}
}
