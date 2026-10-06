package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"komga-lite/internal/model"
)

type Store struct {
	db *sql.DB
}

// Open 打开数据库。PRAGMA 通过 DSN 附加,保证连接池里每个连接都生效
// (WAL 允许读写并行,扫描时前台请求不受阻塞)。conns 为连接池大小(按机器性能传入)。
func Open(path string, conns int) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if conns < 2 {
		conns = 2
	}
	if conns > 8 {
		conns = 8
	}
	dsn := filepath.ToSlash(path) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	// 旧版封面表(book_id 单列主键)→ 丢弃重建;生成封面可随时懒生成,无数据损失
	rows, err := s.db.Query(`PRAGMA table_info(thumbnail)`)
	if err == nil {
		hasKind := false
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil && name == "kind" {
				hasKind = true
			}
		}
		rows.Close()
		if hasKind {
			// 已是新结构
		} else if _, err := s.db.Exec(`DROP TABLE IF EXISTS thumbnail`); err != nil {
			return err
		}
	}
	const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS user (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  email         TEXT NOT NULL UNIQUE,
  name          TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  roles         TEXT NOT NULL DEFAULT 'USER',
  api_key       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS library (
  id   INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  root TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS series (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  library_id          INTEGER NOT NULL REFERENCES library(id) ON DELETE CASCADE,
  name                TEXT NOT NULL,
  created             INTEGER NOT NULL,
  last_modified       INTEGER NOT NULL,
  file_last_modified  INTEGER NOT NULL DEFAULT 0,
  UNIQUE (library_id, name)
);
CREATE INDEX IF NOT EXISTS idx_series_library ON series(library_id);
CREATE TABLE IF NOT EXISTS book (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  series_id          INTEGER NOT NULL REFERENCES series(id) ON DELETE CASCADE,
  name               TEXT NOT NULL,
  number             REAL,
  file_path          TEXT NOT NULL UNIQUE,
  file_size          INTEGER NOT NULL,
  file_mtime         INTEGER NOT NULL,
  file_last_modified INTEGER NOT NULL,
  media_type         TEXT,
  media_status       TEXT NOT NULL,
  pages_count        INTEGER,
  pages_json         TEXT,
  file_hash          TEXT NOT NULL DEFAULT '',
  created            INTEGER NOT NULL,
  last_modified      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_book_series ON book(series_id);
CREATE TABLE IF NOT EXISTS read_progress (
  user_id       INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  book_id       INTEGER NOT NULL REFERENCES book(id) ON DELETE CASCADE,
  page          INTEGER NOT NULL,
  completed     INTEGER NOT NULL DEFAULT 0,
  created       INTEGER NOT NULL DEFAULT 0,
  read_date     INTEGER NOT NULL DEFAULT 0,
  last_modified INTEGER NOT NULL,
  PRIMARY KEY (user_id, book_id)
);
CREATE TABLE IF NOT EXISTS thumbnail (
  book_id    INTEGER NOT NULL REFERENCES book(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL CHECK (kind IN ('GENERATED','USER')),
  data       BLOB NOT NULL,
  media_type TEXT NOT NULL DEFAULT 'image/jpeg',
  PRIMARY KEY (book_id, kind)
);
CREATE TABLE IF NOT EXISTS series_thumbnail (
  series_id   INTEGER PRIMARY KEY REFERENCES series(id) ON DELETE CASCADE,
  data        BLOB NOT NULL,
  media_type  TEXT NOT NULL DEFAULT 'image/jpeg'
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// 旧库升级:列已存在时报错可忽略
	for _, q := range []string{
		`ALTER TABLE series ADD COLUMN file_last_modified INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE book ADD COLUMN file_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE read_progress ADD COLUMN created INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE read_progress ADD COLUMN read_date INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// Secret 返回本实例的会话签名密钥,首次调用时自动生成。
func (s *Store) Secret() ([]byte, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key='session_secret'`).Scan(&v)
	if err == nil {
		return []byte(v), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	v = hex.EncodeToString(buf)
	if _, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES('session_secret', ?)`, v); err != nil {
		return nil, err
	}
	return []byte(v), nil
}

// ---------- library ----------

func (s *Store) UpsertLibrary(name, root string) (int64, error) {
	if _, err := s.db.Exec(
		`INSERT INTO library(name, root) VALUES(?, ?)
		 ON CONFLICT(root) DO UPDATE SET name = excluded.name`, name, root); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRow(`SELECT id FROM library WHERE root = ?`, root).Scan(&id)
	return id, err
}

// DeleteLibrary 删除书库(外键级联删除其系列/书籍/阅读进度/缩略图)。
func (s *Store) DeleteLibrary(id int64) error {
	_, err := s.db.Exec(`DELETE FROM library WHERE id = ?`, id)
	return err
}

// UpdateLibrary 修改书库名称与根目录。
func (s *Store) UpdateLibrary(id int64, name, root string) error {
	res, err := s.db.Exec(`UPDATE library SET name = ?, root = ? WHERE id = ?`, name, root, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return os.ErrNotExist
	}
	return nil
}

func (s *Store) ListLibraries() ([]model.Library, error) {
	rows, err := s.db.Query(`SELECT id, name, root FROM library ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Library
	for rows.Next() {
		var l model.Library
		if err := rows.Scan(&l.ID, &l.Name, &l.Root); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetLibrary(id int64) (*model.Library, error) {
	var l model.Library
	err := s.db.QueryRow(`SELECT id, name, root FROM library WHERE id = ?`, id).Scan(&l.ID, &l.Name, &l.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ---------- user ----------

func (s *Store) CountUsers() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM user`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(email, name, passwordHash, roles string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO user(email, name, password_hash, roles) VALUES(?, ?, ?, ?)`,
		email, name, passwordHash, roles)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanUser(row interface{ Scan(...any) error }) (*model.User, error) {
	var u model.User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Roles, &u.APIKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

const userCols = `id, email, name, password_hash, roles, api_key`

func (s *Store) UserByEmail(email string) (*model.User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM user WHERE email = ?`, email))
}

func (s *Store) UserByID(id int64) (*model.User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM user WHERE id = ?`, id))
}

func (s *Store) UserByAPIKey(key string) (*model.User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM user WHERE api_key = ?`, key))
}

func (s *Store) SetAPIKey(userID int64, key string) error {
	_, err := s.db.Exec(`UPDATE user SET api_key = ? WHERE id = ?`, key, userID)
	return err
}

// ---------- 书籍封面(GENERATED=自动生成 / USER=用户上传) ----------

// ThumbGet 返回当前选中的封面:优先用户上传,其次自动生成。
func (s *Store) ThumbGet(bookID int64) (data []byte, mediaType string, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT data, media_type FROM thumbnail WHERE book_id = ? AND kind = 'USER'`, bookID).Scan(&data, &mediaType)
	if err == nil {
		return data, mediaType, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, err
	}
	err = s.db.QueryRow(
		`SELECT data, media_type FROM thumbnail WHERE book_id = ? AND kind = 'GENERATED'`, bookID).Scan(&data, &mediaType)
	if err == nil {
		return data, mediaType, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, nil
	}
	return nil, "", false, err
}

// ThumbGetKind 返回指定类型的封面。
func (s *Store) ThumbGetKind(bookID int64, kind string) ([]byte, string, bool, error) {
	if kind != "GENERATED" && kind != "USER" {
		return nil, "", false, fmt.Errorf("invalid kind %q", kind)
	}
	var data []byte
	var mt string
	err := s.db.QueryRow(
		`SELECT data, media_type FROM thumbnail WHERE book_id = ? AND kind = ?`, bookID, kind).Scan(&data, &mt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	return data, mt, true, nil
}

// ThumbPutGenerated 写入自动生成封面(不覆盖用户上传)。
func (s *Store) ThumbPutGenerated(bookID int64, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO thumbnail(book_id, kind, data, media_type)
VALUES(?, 'GENERATED', ?, 'image/jpeg')
ON CONFLICT(book_id, kind) DO UPDATE SET data = excluded.data, media_type = excluded.media_type`,
		bookID, data)
	return err
}

// ThumbPutUser 写入用户上传封面(原样保存,成为选中封面)。
func (s *Store) ThumbPutUser(bookID int64, data []byte, mediaType string) error {
	_, err := s.db.Exec(`INSERT INTO thumbnail(book_id, kind, data, media_type)
VALUES(?, 'USER', ?, ?)
ON CONFLICT(book_id, kind) DO UPDATE SET data = excluded.data, media_type = excluded.media_type`,
		bookID, data, mediaType)
	return err
}

// ThumbDeleteUser 删除用户封面(恢复自动生成),返回是否存在。
func (s *Store) ThumbDeleteUser(bookID int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM thumbnail WHERE book_id = ? AND kind = 'USER'`, bookID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ThumbDeleteGenerated 使过期生成封面失效(书籍变化重扫时调用)。
func (s *Store) ThumbDeleteGenerated(bookID int64) error {
	_, err := s.db.Exec(`DELETE FROM thumbnail WHERE book_id = ? AND kind = 'GENERATED'`, bookID)
	return err
}

// ThumbAny 封面(任一类型)是否已存在。
func (s *Store) ThumbAny(bookID int64) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM thumbnail WHERE book_id = ? LIMIT 1`, bookID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// BookRef 用于预热任务。
type BookRef struct {
	ID       int64
	FilePath string
}

// BooksMissingThumb 返回尚无任何封面的可读书籍(预热用,已有用户封面的跳过)。
func (s *Store) BooksMissingThumb(limit int) ([]BookRef, error) {
	rows, err := s.db.Query(`SELECT b.id, b.file_path
FROM book b
WHERE b.media_status = 'READY' AND b.pages_count > 0
  AND NOT EXISTS (SELECT 1 FROM thumbnail t WHERE t.book_id = b.id)
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BookRef
	for rows.Next() {
		var r BookRef
		if err := rows.Scan(&r.ID, &r.FilePath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- 系列封面 ----------

func (s *Store) SeriesThumbGet(seriesID int64) ([]byte, string, bool, error) {
	var data []byte
	var mt string
	err := s.db.QueryRow(`SELECT data, media_type FROM series_thumbnail WHERE series_id = ?`, seriesID).Scan(&data, &mt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	return data, mt, true, nil
}

func (s *Store) SeriesThumbPut(seriesID int64, data []byte, mediaType string) error {
	_, err := s.db.Exec(`INSERT INTO series_thumbnail(series_id, data, media_type) VALUES(?, ?, ?)
ON CONFLICT(series_id) DO UPDATE SET data = excluded.data, media_type = excluded.media_type`,
		seriesID, data, mediaType)
	return err
}

// SeriesThumbDelete 删除系列自定义封面,返回是否存在。
func (s *Store) SeriesThumbDelete(seriesID int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM series_thumbnail WHERE series_id = ?`, seriesID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func now() int64 { return time.Now().UTC().Unix() }
