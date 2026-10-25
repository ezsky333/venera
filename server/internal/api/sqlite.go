package api

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// openSQLiteReadOnly 以只读 + immutable 方式打开 App 导出的 SQLite 文件。
//
// 用 immutable=1 的理由：这些是从 .venera 包里解出来的静态副本，
// 不会有并发写入；immutable 可避免 SQLite 尝试创建 -wal/-shm 文件。
// 密码/加密的数据库不在支持范围内（App 本身也不加密）。
func openSQLiteReadOnly(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	return db, nil
}
