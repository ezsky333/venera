package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
)

func timeNow() time.Time { return time.Now() }

func modelComic(src, id, title, cover string) model.Comic {
	return model.Comic{Source: src, ID: id, Title: title, Cover: cover}
}

// TestRefetchCoversOnlyTouchesMissing 锁住封面补抓的两个语义：
//
//  1. 只有"缺 cover 文件"的漫画才被抓取（已有 cover 的不重下）；
//  2. 补抓成功后 cover 文件确实落盘。
//
// 背景：picacg 封面 301 事故留下 105 本"有内页无封面"的存量漫画，
// RefetchCovers 是为它们准备的轻量入口，绝不能把正常漫画的封面重抓一遍。
func TestRefetchCoversOnlyTouchesMissing(t *testing.T) {
	st := newTestStore(t)

	const (
		dirA = "Comic A [aaaaaaaa]"
		dirB = "Comic B [bbbbbbbb]"
	)
	lib := st.LibraryDir()
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(filepath.Join(lib, d, "1"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// B 的 cover 已存在
	if err := os.WriteFile(filepath.Join(lib, dirB, "cover.jpg"), pngBytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// 登记章节缓存（有章节才算"已缓存漫画"）
	for _, c := range []struct{ src, cid, dir string }{
		{"s", "a", dirA + "/1"},
		{"s", "b", dirB + "/1"},
	} {
		if err := st.PutCachedChapter(model.CachedChapter{
			Source: c.src, ComicID: c.cid, ChapterID: "1",
			Dir: c.dir, CreatedAt: timeNow(),
		}); err != nil {
			t.Fatalf("PutCachedChapter: %v", err)
		}
	}
	// comics 元信息（含封面 URL）
	_ = st.UpsertComic(modelComic("s", "a", "Comic A", "https://img.example/a.jpg"), 0)
	_ = st.UpsertComic(modelComic("s", "b", "Comic B", "https://img.example/b.jpg"), 0)

	var fetched []string
	dl := NewDownloader(st, &fakeRunner{}, 2)
	dl.SetFetch(func(url string, _ map[string]string) ([]byte, error) {
		fetched = append(fetched, url)
		return pngBytes(), nil
	})

	tried, ok, err := dl.RefetchCovers()
	if err != nil {
		t.Fatalf("RefetchCovers: %v", err)
	}
	if tried != 1 || ok != 1 {
		t.Fatalf("tried=%d ok=%d, want 1/1 (only missing cover)", tried, ok)
	}
	if len(fetched) != 1 || fetched[0] != "https://img.example/a.jpg" {
		t.Fatalf("fetched=%v, want exactly [a.jpg]", fetched)
	}
	// A 的 cover 落盘了
	if !coverExists(filepath.Join(lib, dirA)) {
		t.Fatal("cover for A must exist after refetch")
	}
}

// TestRefetchCoversKeepsFailureVisible 确认补抓失败时不假装成功：
// ok 计数不包含失败项，方便调用方判断还剩多少缺口。
func TestRefetchCoversKeepsFailureVisible(t *testing.T) {
	st := newTestStore(t)
	lib := st.LibraryDir()
	dir := "Comic C [cccccccc]"
	if err := os.MkdirAll(filepath.Join(lib, dir, "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCachedChapter(model.CachedChapter{
		Source: "s", ComicID: "c", ChapterID: "1",
		Dir: dir + "/1", CreatedAt: timeNow(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertComic(modelComic("s", "c", "Comic C", "https://img.example/c.jpg"), 0)

	dl := NewDownloader(st, &fakeRunner{}, 2)
	dl.SetFetch(func(string, map[string]string) ([]byte, error) {
		return nil, fmt.Errorf("image fetch: status 500")
	})

	tried, ok, err := dl.RefetchCovers()
	if err != nil {
		t.Fatalf("RefetchCovers: %v", err)
	}
	if tried != 1 || ok != 0 {
		t.Fatalf("tried=%d ok=%d, want 1/0 on persistent failure", tried, ok)
	}
	if coverExists(filepath.Join(lib, dir)) {
		t.Fatal("failed refetch must not leave a cover file")
	}
}
