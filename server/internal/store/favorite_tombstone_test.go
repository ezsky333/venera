package store

import (
	"errors"
	"testing"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
)

func favModel(folder, comicID string) model.Favorite {
	return model.Favorite{
		Comic:   model.Comic{Source: "jm", ID: comicID, Title: comicID},
		Folder:  folder,
		AddedAt: time.Now(),
	}
}

func hasFav(t *testing.T, s *Store, folder, comicID string) bool {
	t.Helper()
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM favorites WHERE folder = ? AND source = ? AND comic_id = ?`,
		folder, "jm", comicID).Scan(&n)
	if err != nil {
		t.Fatalf("count favorite: %v", err)
	}
	return n > 0
}

// 迟到的 fav.add（lamport 比删除更早）必须被墓碑挡住。
//
// 这是"旧 add 复活"的核心场景：设备离线时队列里压着一条 fav.add，
// 另一台设备删除了该收藏，离线设备恢复后把旧 add 推上来。
func TestAddFavoriteBlockedByNewerTombstone(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.AddFavorite(favModel("夹", "X"), 1000); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	if !hasFav(t, s, "夹", "X") {
		t.Fatal("seed favorite missing")
	}

	if _, err := s.RemoveFavorite("夹", "jm", "X", 2000); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if hasFav(t, s, "夹", "X") {
		t.Fatal("favorite still present after remove")
	}

	// 旧 add（lamport 1000 < 墓碑 2000）必须被拒
	changed, err := s.AddFavorite(favModel("夹", "X"), 1000)
	if !errors.Is(err, ErrFavoriteResurrect) {
		t.Fatalf("err = %v, want ErrFavoriteResurrect", err)
	}
	if changed {
		t.Error("changed = true, want false")
	}
	if hasFav(t, s, "夹", "X") {
		t.Fatal("stale add resurrected the favorite")
	}
}

// 删除之后用户重新收藏（lamport 更新）必须成功，并清掉墓碑。
func TestAddFavoriteAfterTombstoneWhenNewer(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.AddFavorite(favModel("夹", "X"), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveFavorite("夹", "jm", "X", 2000); err != nil {
		t.Fatal(err)
	}

	changed, err := s.AddFavorite(favModel("夹", "X"), 3000)
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true")
	}
	if !hasFav(t, s, "夹", "X") {
		t.Fatal("favorite not restored")
	}

	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM favorite_tombstones
		 WHERE folder = '夹' AND source = 'jm' AND comic_id = 'X'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("tombstone still present (%d rows), want cleared", n)
	}
}

// lamport 相等的边界：视为"删除已经发生"，拒绝旧的 add。
// 用 >= 而不是 > 是刻意的——同一毫秒内到达的 add 不能复活删除。
func TestAddFavoriteBlockedOnEqualLamport(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AddFavorite(favModel("夹", "X"), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveFavorite("夹", "jm", "X", 2000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFavorite(favModel("夹", "X"), 2000); !errors.Is(err, ErrFavoriteResurrect) {
		t.Fatalf("err = %v, want ErrFavoriteResurrect on equal lamport", err)
	}
	if hasFav(t, s, "夹", "X") {
		t.Fatal("equal-lamport add resurrected the favorite")
	}
}

// 墓碑不能挡住"另一个文件夹"的同名漫画：收藏的唯一键是
// (folder, source, comic_id)，跨文件夹互不影响。
func TestTombstoneIsPerFolder(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AddFavorite(favModel("A夹", "X"), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveFavorite("A夹", "jm", "X", 2000); err != nil {
		t.Fatal(err)
	}
	// 加到另一个文件夹，不该被 A夹 的墓碑影响
	changed, err := s.AddFavorite(favModel("B夹", "X"), 1000)
	if err != nil {
		t.Fatalf("add to other folder: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true")
	}
	if !hasFav(t, s, "B夹", "X") {
		t.Fatal("favorite missing in other folder")
	}
}

// 重复 remove 不能让墓碑时间戳变旧（否则更晚的旧 add 就能钻空子）。
func TestRemoveFavoriteKeepsNewestTombstoneLamport(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AddFavorite(favModel("夹", "X"), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveFavorite("夹", "jm", "X", 5000); err != nil {
		t.Fatal(err)
	}
	// 一条迟到的、更旧的 remove
	if _, err := s.RemoveFavorite("夹", "jm", "X", 3000); err != nil {
		t.Fatal(err)
	}

	var lamport int64
	if err := s.db.QueryRow(
		`SELECT lamport FROM favorite_tombstones
		 WHERE folder = '夹' AND source = 'jm' AND comic_id = 'X'`).Scan(&lamport); err != nil {
		t.Fatal(err)
	}
	if lamport != 5000 {
		t.Fatalf("tombstone lamport = %d, want 5000", lamport)
	}

	// lamport 4000 的 add 仍应被拒（墓碑是 5000）
	if _, err := s.AddFavorite(favModel("夹", "X"), 4000); !errors.Is(err, ErrFavoriteResurrect) {
		t.Fatalf("err = %v, want ErrFavoriteResurrect", err)
	}
}

// 移动到别的文件夹后，源文件夹的墓碑要挡住迟到的旧 add，
// 否则那条 add 会把它同时塞回源文件夹（表现为"移动没生效/重复出现"）。
func TestMoveFavoriteWritesSourceTombstone(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AddFavorite(favModel("源", "X"), 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveFavorite(favModel("目标", "X"), "源", 2000); err != nil {
		t.Fatalf("move: %v", err)
	}
	if hasFav(t, s, "源", "X") {
		t.Fatal("still in source folder after move")
	}
	if !hasFav(t, s, "目标", "X") {
		t.Fatal("missing in target folder after move")
	}

	// 迟到的旧 add 指向源文件夹
	if _, err := s.AddFavorite(favModel("源", "X"), 1000); !errors.Is(err, ErrFavoriteResurrect) {
		t.Fatalf("err = %v, want ErrFavoriteResurrect", err)
	}
	if hasFav(t, s, "源", "X") {
		t.Fatal("stale add put it back into source folder")
	}
}
