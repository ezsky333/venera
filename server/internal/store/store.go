package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/venera-app/venera-nas/internal/model"
)

// Store 是所有持久化状态的入口：SQLite 承载元数据，文件系统承载图片。
type Store struct {
	db         *sql.DB
	dataDir    string
	libraryDir string
}

// Open 打开（并按需建库建表）存储。
func Open(dataDir, libraryDir string) (*Store, error) {
	// 目录必须先存在，否则 SQLite 报 "unable to open database file"。
	// 由 Store 自己保证，避免调用方（含测试）踩到这个坑。
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		return nil, fmt.Errorf("create library dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "nas.db")
	// WAL 允许读写并发；busy_timeout 避免瞬时锁冲突直接报错。
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// WAL 模式下读可以并发、写是库级独占，因此：
	//   - 多开几个连接让读请求并行（UI/API 查询不会被下载任务卡住）
	//   - busy_timeout 让写冲突自动排队而不是直接报 SQLITE_BUSY
	// 注意：不要把连接池设成 1，否则 handler 内的嵌套查询会互相等待死锁。
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, dataDir: dataDir, libraryDir: libraryDir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB        { return s.db }
func (s *Store) DataDir() string    { return s.dataDir }
func (s *Store) LibraryDir() string { return s.libraryDir }

// Videos/Images 的落盘根目录，与 App 的 LocalManager 布局保持一致。
func (s *Store) LibraryPath(rel string) string {
	return filepath.Join(s.libraryDir, filepath.FromSlash(rel))
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sources (
			key         TEXT PRIMARY KEY,
			name        TEXT,
			version     TEXT,
			file        TEXT,
			installed_at INTEGER,
			enabled     INTEGER DEFAULT 1
		)`,
		// 源私有数据（登录 token 等），对应 App 的 <key>.data
		`CREATE TABLE IF NOT EXISTS source_data (
			source_key TEXT PRIMARY KEY,
			data       TEXT NOT NULL,
			updated_at INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS source_cookies (
			source_key TEXT NOT NULL,
			name       TEXT NOT NULL,
			value      TEXT NOT NULL,
			domain     TEXT NOT NULL,
			path       TEXT NOT NULL DEFAULT '/',
			expires    INTEGER,
			secure     INTEGER DEFAULT 0,
			http_only  INTEGER DEFAULT 0,
			PRIMARY KEY (source_key, name, domain, path)
		)`,
		// sourceKey -> hashCode 映射，避免长期依赖 Dart 哈希算法
		`CREATE TABLE IF NOT EXISTS source_type_map (
			source_key TEXT PRIMARY KEY,
			source_type INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS comics (
			source      TEXT NOT NULL,
			comic_id    TEXT NOT NULL,
			title       TEXT,
			subtitle    TEXT,
			cover       TEXT,
			tags        TEXT,
			source_type INTEGER,
			updated_at  INTEGER,
			PRIMARY KEY (source, comic_id)
		)`,
		`CREATE TABLE IF NOT EXISTS favorites (
			folder      TEXT NOT NULL,
			source      TEXT NOT NULL,
			comic_id    TEXT NOT NULL,
			title       TEXT,
			author      TEXT,
			cover       TEXT,
			tags        TEXT,
			source_type INTEGER,
			added_at    INTEGER,
			display_order INTEGER DEFAULT 0,
			lamport     INTEGER DEFAULT 0,
			PRIMARY KEY (folder, source, comic_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_favorites_comic ON favorites(source, comic_id)`,
		// 收藏墓碑：记录"这一项在 lamport 时刻被删除了"。
		//
		// 为什么必须有：favorites 是集合语义，删除就是一行 DELETE。
		// 一旦行没了，就再也分不清"从来没收藏过"和"收藏过又删了"。
		// 于是任何一条迟到的 fav.add（离线队列、重装后的 bootstrap 全量重推、
		// 别的设备还没同步到删除）都会把已删除的收藏重新加回来——
		// 实测确认过：remove 之后再推一条 add，收藏确实复活。
		//
		// 墓碑保留删除时刻的 lamport，AddFavorite 据此判断这条 add 是
		// "删除之前就发出的旧消息"（拒绝）还是"用户后来重新收藏"（接受）。
		// 接受时把墓碑删掉，否则表会无限增长。
		`CREATE TABLE IF NOT EXISTS favorite_tombstones (
			folder      TEXT NOT NULL,
			source      TEXT NOT NULL,
			comic_id    TEXT NOT NULL,
			lamport     INTEGER NOT NULL DEFAULT 0,
			deleted_at  INTEGER,
			PRIMARY KEY (folder, source, comic_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tombstones_comic
			ON favorite_tombstones(source, comic_id)`,
		`CREATE TABLE IF NOT EXISTS histories (
			source      TEXT NOT NULL,
			comic_id    TEXT NOT NULL,
			title       TEXT,
			cover       TEXT,
			source_type INTEGER,
			ep          INTEGER DEFAULT 0,
			page        INTEGER DEFAULT 0,
			"group"     INTEGER DEFAULT 0,
			read_eps    TEXT,
			max_page    INTEGER,
			updated_at  INTEGER,
			lamport     INTEGER DEFAULT 0,
			PRIMARY KEY (source, comic_id)
		)`,
		// 已缓存章节：客户端据此判断"NAS 是否已有这一章"
		`CREATE TABLE IF NOT EXISTS cached_chapters (
			source        TEXT NOT NULL,
			comic_id      TEXT NOT NULL,
			chapter_id    TEXT NOT NULL,
			chapter_title TEXT,
			dir           TEXT NOT NULL,
			page_count    INTEGER DEFAULT 0,
			cover         TEXT,
			bytes         INTEGER DEFAULT 0,
			created_at    INTEGER,
			PRIMARY KEY (source, comic_id, chapter_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cached_comic ON cached_chapters(source, comic_id)`,
		`CREATE TABLE IF NOT EXISTS jobs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			source      TEXT NOT NULL,
			comic_id    TEXT NOT NULL,
			title       TEXT,
			chapters    TEXT,
			status      TEXT NOT NULL,
			progress    REAL DEFAULT 0,
			message     TEXT,
			error       TEXT,
			origin      TEXT,
			attempts    INTEGER DEFAULT 0,
			created_at  INTEGER,
			updated_at  INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status)`,
		// 同步操作日志：W2 协议的核心，客户端以 id 为游标增量拉取
		`CREATE TABLE IF NOT EXISTS ops (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			client_id  TEXT NOT NULL,
			op_type    TEXT NOT NULL,
			lamport    INTEGER NOT NULL,
			payload    TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ops_client ON ops(client_id)`,
		`CREATE TABLE IF NOT EXISTS clients (
			client_id    TEXT PRIMARY KEY,
			device_name  TEXT,
			last_seen_at INTEGER,
			last_op_id   INTEGER DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate %q: %w", q[:min(40, len(q))], err)
		}
	}
	// 增量列：CREATE TABLE IF NOT EXISTS 不会给已存在的表补列。
	if err := s.ensureColumn("jobs", "attempts", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	// display_order 用于跨设备保持收藏顺序（客户端列表按它排序）
	if err := s.ensureColumn("favorites", "display_order", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	// chapter_list 保存**源端完整章节列表**（JSON 数组，按源端顺序）。
	//
	// 为什么需要它：离线兜底详情页原本只返回"已缓存的章节"，于是客户端
	// 拿到的章节表是**被压缩过的**——本地 history.ep=19 指的是源端的第 19 章，
	// 在压缩表里却指向第 19 个"已缓存"的章节，阅读器就会打开错的那一章
	// （实测表现：历史显示 E39，详情页显示第 19 章）。
	//
	// 有了这一列，兜底时能重建与源端一致的下标，未缓存的章节标记出来
	// 交给客户端灰掉，索引关系就永远对得上了。
	if err := s.ensureColumn("comics", "chapter_list", "TEXT"); err != nil {
		return err
	}
	// jobs.priority 支持"置顶"：数值大的先被 worker 领取。
	if err := s.ensureColumn("jobs", "priority", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	// comics 的展示字段：NAS 兜底详情页需要它们才不会丢作者/简介。
	if err := s.ensureColumn("comics", "author", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn("comics", "uploader", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn("comics", "description", "TEXT"); err != nil {
		return err
	}
	return nil
}

// ensureColumn 在缺列时给表补上一列（幂等）。
func (s *Store) ensureColumn(table, column, decl string) error {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notNull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- source type map ----------

// SourceType 返回与 App 一致的 sourceKey.hashCode。
// 优先读映射表（由客户端上报，长期稳定），否则现场计算并缓存。
func (s *Store) SourceType(sourceKey string, computed int32) int32 {
	var v int32
	err := s.db.QueryRow(`SELECT source_type FROM source_type_map WHERE source_key = ?`, sourceKey).Scan(&v)
	if err == nil {
		return v
	}
	_, _ = s.db.Exec(`INSERT OR REPLACE INTO source_type_map(source_key, source_type) VALUES (?, ?)`, sourceKey, computed)
	return computed
}

// PutSourceType 由客户端上报权威值覆盖本地计算。
func (s *Store) PutSourceType(sourceKey string, v int32) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO source_type_map(source_key, source_type) VALUES (?, ?)`, sourceKey, v)
	return err
}

// ---------- ops ----------

// AppendOp 追加一条操作并返回其全局游标。
func (s *Store) AppendOp(clientID string, opType string, lamport int64, payload any) (int64, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(
		`INSERT INTO ops(client_id, op_type, lamport, payload, created_at) VALUES (?, ?, ?, ?, ?)`,
		clientID, opType, lamport, string(b), time.Now().UnixMilli(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// OpsSince 返回游标之后的操作，按 id 升序。
func (s *Store) OpsSince(cursor int64, limit int, excludeClient string) ([]model.Op, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	q := `SELECT id, client_id, op_type, lamport, payload, created_at FROM ops WHERE id > ?`
	args := []any{cursor}
	if excludeClient != "" {
		// 回显抑制：不把某客户端自己的操作再推回去（由服务端状态快照保证一致性）
		q += ` AND client_id <> ?`
		args = append(args, excludeClient)
	}
	q += ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Op
	for rows.Next() {
		var op model.Op
		var payload string
		var created int64
		if err := rows.Scan(&op.ID, &op.ClientID, &op.Type, &op.Lamport, &payload, &created); err != nil {
			return nil, err
		}
		op.Payload = json.RawMessage(payload)
		op.At = time.UnixMilli(created)
		out = append(out, op)
	}
	return out, rows.Err()
}

// MaxOpID 返回当前游标上界。
func (s *Store) MaxOpID() (int64, error) {
	var v sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(id) FROM ops`).Scan(&v); err != nil {
		return 0, err
	}
	return v.Int64, nil
}

// TrimOps 删除过期操作。注意：客户端若游标落后于此，需要做全量重同步。
func (s *Store) TrimOps(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM ops WHERE created_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- settings & clients ----------

func (s *Store) PutSetting(key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO settings(key, value, updated_at) VALUES (?, ?, ?)`,
		key, string(b), time.Now().UnixMilli())
	return err
}

func (s *Store) GetSetting(key string, out any) (bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), out)
}

func (s *Store) AllSettings() (map[string]json.RawMessage, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}

func (s *Store) TouchClient(clientID, name string, cursor int64) error {
	_, err := s.db.Exec(
		`INSERT INTO clients(client_id, device_name, last_seen_at, last_op_id) VALUES (?, ?, ?, ?)
		 ON CONFLICT(client_id) DO UPDATE SET last_seen_at = excluded.last_seen_at,
		   device_name = COALESCE(NULLIF(excluded.device_name, ''), clients.device_name),
		   last_op_id = MAX(clients.last_op_id, excluded.last_op_id)`,
		clientID, name, time.Now().UnixMilli(), cursor,
	)
	return err
}

func (s *Store) DeleteClientOps(clientID string) error {
	_, err := s.db.Exec(`DELETE FROM ops WHERE client_id = ?`, clientID)
	return err
}

func (s *Store) EnsureDirs() error {
	for _, d := range []string{
		filepath.Join(s.dataDir, "sources"),
		filepath.Join(s.dataDir, "snapshots"),
		s.libraryDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
