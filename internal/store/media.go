package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"komga-lite/internal/model"
)

type Sort struct {
	Key  string
	Desc bool
}

// ---------- series ----------

type SeriesQuery struct {
	UserID     int64
	LibraryIDs []int64
	Search     string
	ReadStatus []string // READ / UNREAD / IN_PROGRESS
	Page, Size int
	Sorts      []Sort
}

// seriesInner 生成带阅读计数的内层查询,返回 SQL 与基础参数(uid 出现两次)。
func seriesInner(uid int64) (string, []any) {
	sqlStr := `SELECT s.id, s.library_id, s.name, s.created, s.last_modified, s.file_last_modified,
  (SELECT COUNT(*) FROM book b WHERE b.series_id = s.id) AS books_count,
  (SELECT COUNT(*) FROM book b JOIN read_progress rp ON rp.book_id = b.id AND rp.user_id = ?
     WHERE b.series_id = s.id AND rp.completed = 1) AS read_count,
  (SELECT COUNT(*) FROM book b JOIN read_progress rp ON rp.book_id = b.id AND rp.user_id = ?
     WHERE b.series_id = s.id AND rp.completed = 0 AND rp.page > 0) AS progress_count
FROM series s`
	return sqlStr, []any{uid, uid}
}

func scanSeries(scan interface{ Scan(...any) error }) (*model.Series, error) {
	var s model.Series
	var created, modified, flm int64
	if err := scan.Scan(&s.ID, &s.LibraryID, &s.Name, &created, &modified, &flm,
		&s.BookCount, &s.ReadCount, &s.InProgressCount); err != nil {
		return nil, err
	}
	s.Created = time.Unix(created, 0).UTC()
	s.LastModified = time.Unix(modified, 0).UTC()
	s.FileLastModified = time.Unix(flm, 0).UTC()
	return &s, nil
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

func seriesOrder(sorts []Sort) string {
	expr := map[string]string{
		"name": "t.name COLLATE NOCASE", "title": "t.name COLLATE NOCASE",
		"metadata.titleSort": "t.name COLLATE NOCASE",
		"created":            "t.created",
		"lastModified":       "t.last_modified",
		"booksCount":         "t.books_count",
	}
	parts := make([]string, 0, len(sorts)+1)
	for _, so := range sorts {
		e, ok := expr[so.Key]
		if !ok {
			continue
		}
		if so.Desc {
			e += " DESC"
		}
		parts = append(parts, e)
	}
	if len(parts) == 0 {
		parts = append(parts, "t.name COLLATE NOCASE ASC")
	}
	parts = append(parts, "t.id ASC") // 稳定排序
	return strings.Join(parts, ", ")
}

func readStatusCond(rs []string) string {
	var conds []string
	for _, s := range rs {
		switch strings.ToUpper(strings.TrimSpace(s)) {
		case "READ":
			conds = append(conds, "(t.read_count = t.books_count AND t.books_count > 0)")
		case "UNREAD":
			conds = append(conds, "(t.read_count = 0 AND t.progress_count = 0)")
		case "IN_PROGRESS":
			conds = append(conds, "(t.progress_count > 0 AND t.read_count < t.books_count)")
		}
	}
	if len(conds) == 0 {
		return ""
	}
	return strings.Join(conds, " OR ")
}

func (s *Store) ListSeries(q SeriesQuery) ([]model.Series, int64, error) {
	inner, args := seriesInner(q.UserID)
	where := []string{"1=1"}
	if len(q.LibraryIDs) > 0 {
		ph := make([]string, len(q.LibraryIDs))
		for i, id := range q.LibraryIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		where = append(where, "s.library_id IN ("+strings.Join(ph, ",")+")")
	}
	if q.Search != "" {
		where = append(where, `lower(s.name) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(strings.ToLower(q.Search))+"%")
	}
	inner += " WHERE " + strings.Join(where, " AND ")

	var outerWhere string
	if c := readStatusCond(q.ReadStatus); c != "" {
		outerWhere = " WHERE " + c
	}

	var total int64
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM (`+inner+`) t`+outerWhere, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	sqlStr := `SELECT t.id, t.library_id, t.name, t.created, t.last_modified, t.file_last_modified,
  t.books_count, t.read_count, t.progress_count
FROM (` + inner + `) t` + outerWhere + `
ORDER BY ` + seriesOrder(q.Sorts) + `
LIMIT ? OFFSET ?`
	args = append(args, q.Size, q.Page*q.Size)
	rows, err := s.db.Query(sqlStr, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.Series
	for rows.Next() {
		sr, err := scanSeries(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *sr)
	}
	return out, total, rows.Err()
}

func (s *Store) GetSeries(uid, id int64) (*model.Series, error) {
	inner, args := seriesInner(uid)
	args = append(args, id)
	row := s.db.QueryRow(
		`SELECT t.id, t.library_id, t.name, t.created, t.last_modified, t.file_last_modified,
  t.books_count, t.read_count, t.progress_count
FROM (`+inner+` WHERE s.id = ?) t`, args...)
	sr, err := scanSeries(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sr, nil
}

// FirstReadyBookID 返回系列中按册序排列的第一本可读书籍 id(用作系列封面来源)。
func (s *Store) FirstReadyBookID(seriesID int64) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM book WHERE series_id = ? AND media_status = 'READY' AND pages_count > 0
		 ORDER BY CASE WHEN number IS NULL THEN 1 ELSE 0 END, number, name COLLATE NOCASE
		 LIMIT 1`, seriesID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (s *Store) UpsertSeries(libraryID int64, name string, fileLastModified int64) (int64, error) {
	ts := now()
	if _, err := s.db.Exec(
		`INSERT INTO series(library_id, name, created, last_modified, file_last_modified)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(library_id, name) DO UPDATE SET file_last_modified = excluded.file_last_modified`,
		libraryID, name, ts, ts, fileLastModified); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRow(`SELECT id FROM series WHERE library_id = ? AND name = ?`, libraryID, name).Scan(&id)
	return id, err
}

func (s *Store) DeleteMissingSeries(libraryID int64, keepNames []string) error {
	keep := make(map[string]bool, len(keepNames))
	for _, n := range keepNames {
		keep[n] = true
	}
	rows, err := s.db.Query(`SELECT id, name FROM series WHERE library_id = ?`, libraryID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if !keep[name] {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM series WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------- book ----------

func bookOrder(sorts []Sort) string {
	expr := map[string]string{
		"number":           "CASE WHEN b.number IS NULL THEN 1 ELSE 0 END, b.number",
		"name":             "b.name COLLATE NOCASE",
		"created":          "b.created",
		"lastModified":     "b.last_modified",
		"fileLastModified": "b.file_last_modified",
		"fileSize":         "b.file_size",
		"sizeBytes":        "b.file_size",
	}
	parts := make([]string, 0, len(sorts)+2)
	for _, so := range sorts {
		e, ok := expr[so.Key]
		if !ok {
			continue
		}
		if so.Desc {
			e += " DESC"
		}
		parts = append(parts, e)
	}
	if len(parts) == 0 {
		parts = append(parts,
			"CASE WHEN b.number IS NULL THEN 1 ELSE 0 END ASC",
			"b.number ASC", "b.name COLLATE NOCASE ASC")
	}
	parts = append(parts, "b.id ASC")
	return strings.Join(parts, ", ")
}

const bookCols = `b.id, b.series_id, s.library_id, s.name AS series_title, b.name, b.number,
  b.file_path, b.file_size, b.file_last_modified, b.media_type, b.media_status,
  b.pages_count, b.pages_json, b.file_hash, b.created, b.last_modified,
  rp.page, rp.completed, rp.created, rp.read_date, rp.last_modified`

func scanBook(scan interface{ Scan(...any) error }) (*model.Book, error) {
	var b model.Book
	var flm, created, modified int64
	var pgPage, pgCreated, pgRead, pgLM sql.NullInt64
	var pgDone sql.NullInt64
	if err := scan.Scan(&b.ID, &b.SeriesID, &b.LibraryID, &b.SeriesName, &b.Name, &b.Number,
		&b.FilePath, &b.FileSize, &flm, &b.MediaType, &b.MediaStatus,
		&b.PagesCount, &b.PagesJSON, &b.FileHash, &created, &modified,
		&pgPage, &pgDone, &pgCreated, &pgRead, &pgLM); err != nil {
		return nil, err
	}
	b.FileLastModified = time.Unix(flm, 0).UTC()
	b.Created = time.Unix(created, 0).UTC()
	b.LastModified = time.Unix(modified, 0).UTC()
	if pgPage.Valid {
		b.Progress = &model.ReadProgress{
			BookID:       b.ID,
			Page:         int(pgPage.Int64),
			Completed:    pgDone.Int64 == 1,
			Created:      time.Unix(pgCreated.Int64, 0).UTC(),
			ReadDate:     time.Unix(pgRead.Int64, 0).UTC(),
			LastModified: time.Unix(pgLM.Int64, 0).UTC(),
		}
	}
	return &b, nil
}

func (s *Store) ListBooks(uid, seriesID int64, page, size int, sorts []Sort) ([]model.Book, int64, error) {
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM book WHERE series_id = ?`, seriesID).Scan(&total); err != nil {
		return nil, 0, err
	}
	sqlStr := `SELECT ` + bookCols + `
FROM book b
JOIN series s ON s.id = b.series_id
LEFT JOIN read_progress rp ON rp.book_id = b.id AND rp.user_id = ?
WHERE b.series_id = ?
ORDER BY ` + bookOrder(sorts) + `
LIMIT ? OFFSET ?`
	rows, err := s.db.Query(sqlStr, uid, seriesID, size, page*size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *b)
	}
	return out, total, rows.Err()
}

func (s *Store) GetBook(uid, id int64) (*model.Book, error) {
	row := s.db.QueryRow(`SELECT `+bookCols+`
FROM book b
JOIN series s ON s.id = b.series_id
LEFT JOIN read_progress rp ON rp.book_id = b.id AND rp.user_id = ?
WHERE b.id = ?`, uid, id)
	b, err := scanBook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get book: %w", err)
	}
	return b, nil
}

// ---------- 全量书籍列表(/api/v1/books,Koharia 用于全库同步) ----------

type BookQuery struct {
	UserID     int64
	LibraryIDs []int64
	SeriesIDs  []int64
	Search     string
	ReadStatus []string // READ / UNREAD / IN_PROGRESS
	Page, Size int
	Sorts      []Sort
}

func globalBookOrder(sorts []Sort) string {
	expr := map[string]string{
		"name":             "b.name COLLATE NOCASE",
		"title":            "b.name COLLATE NOCASE",
		"number":           "CASE WHEN b.number IS NULL THEN 1 ELSE 0 END, b.number",
		"created":          "b.created",
		"lastModified":     "b.last_modified",
		"fileLastModified": "b.file_last_modified",
		"fileSize":         "b.file_size",
		"sizeBytes":        "b.file_size",
		"releaseDate":      "b.created",
	}
	parts := make([]string, 0, len(sorts)+4)
	for _, so := range sorts {
		e, ok := expr[so.Key]
		if !ok {
			continue
		}
		if so.Desc {
			e += " DESC"
		}
		parts = append(parts, e)
	}
	if len(parts) == 0 {
		parts = append(parts,
			"s.name COLLATE NOCASE ASC",
			"CASE WHEN b.number IS NULL THEN 1 ELSE 0 END ASC",
			"b.number ASC", "b.name COLLATE NOCASE ASC")
	}
	parts = append(parts, "b.id ASC") // 稳定排序
	return strings.Join(parts, ", ")
}

func (s *Store) ListAllBooks(q BookQuery) ([]model.Book, int64, error) {
	base := `FROM book b
JOIN series s ON s.id = b.series_id
LEFT JOIN read_progress rp ON rp.book_id = b.id AND rp.user_id = ?`
	where := []string{"1=1"}
	args := []any{q.UserID}
	if len(q.LibraryIDs) > 0 {
		ph := make([]string, len(q.LibraryIDs))
		for i, id := range q.LibraryIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		where = append(where, "s.library_id IN ("+strings.Join(ph, ",")+")")
	}
	if len(q.SeriesIDs) > 0 {
		ph := make([]string, len(q.SeriesIDs))
		for i, id := range q.SeriesIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		where = append(where, "b.series_id IN ("+strings.Join(ph, ",")+")")
	}
	if q.Search != "" {
		where = append(where, `(lower(b.name) LIKE ? ESCAPE '\' OR lower(s.name) LIKE ? ESCAPE '\')`)
		pat := "%" + escapeLike(strings.ToLower(q.Search)) + "%"
		args = append(args, pat, pat)
	}
	var rs []string
	for _, s := range q.ReadStatus {
		switch strings.ToUpper(strings.TrimSpace(s)) {
		case "READ":
			rs = append(rs, "rp.completed = 1")
		case "UNREAD":
			rs = append(rs, "rp.page IS NULL")
		case "IN_PROGRESS":
			rs = append(rs, "(rp.completed = 0 AND rp.page > 0)")
		}
	}
	if len(rs) > 0 {
		where = append(where, "("+strings.Join(rs, " OR ")+")")
	}
	cond := " WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) `+base+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT `+bookCols+` `+base+cond+`
ORDER BY `+globalBookOrder(q.Sorts)+`
LIMIT ? OFFSET ?`, append(args, q.Size, q.Page*q.Size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *b)
	}
	return out, total, rows.Err()
}

// ---------- 扫描器写入 ----------

// BookStat 返回文件当前入库信息;未入库时 id 为 0。
func (s *Store) BookStat(filePath string) (id, size, mtime int64, err error) {
	err = s.db.QueryRow(`SELECT id, file_size, file_mtime FROM book WHERE file_path = ?`, filePath).
		Scan(&id, &size, &mtime)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, nil
	}
	return id, size, mtime, err
}

type BookUp struct {
	SeriesID         int64
	Name             string
	Number           sql.NullFloat64
	FilePath         string
	FileSize         int64
	FileMTime        int64
	FileLastModified int64
	MediaType        sql.NullString
	MediaStatus      string
	PagesCount       sql.NullInt64
	PagesJSON        sql.NullString
	FileHash         string
}

func (s *Store) UpsertBook(b BookUp) error {
	ts := now()
	_, err := s.db.Exec(`INSERT INTO book(series_id, name, number, file_path, file_size, file_mtime,
  file_last_modified, media_type, media_status, pages_count, pages_json, file_hash, created, last_modified)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(file_path) DO UPDATE SET
  series_id = excluded.series_id, name = excluded.name, number = excluded.number,
  file_size = excluded.file_size, file_mtime = excluded.file_mtime,
  file_last_modified = excluded.file_last_modified, media_type = excluded.media_type,
  media_status = excluded.media_status, pages_count = excluded.pages_count,
  pages_json = excluded.pages_json, file_hash = excluded.file_hash, last_modified = excluded.last_modified`,
		b.SeriesID, b.Name, b.Number, b.FilePath, b.FileSize, b.FileMTime, b.FileLastModified,
		b.MediaType, b.MediaStatus, b.PagesCount, b.PagesJSON, b.FileHash, ts, ts)
	return err
}

// BookWrite 是批量入库的一条记录;InvalidateThumb 在书籍有变化时清理过期的生成封面。
type BookWrite struct {
	Up              BookUp
	InvalidateThumb bool
}

// FlushBooks 批量事务写库(扫描器 writer 协程使用),减少每条语句一次落盘的开销。
// 批次失败时降级为逐行写入:系列/书库已被删除的行(FK 失败)跳过,
// 避免扫描期间书库被删导致整批书籍丢失。
func (s *Store) FlushBooks(batch []BookWrite) error {
	if len(batch) == 0 {
		return nil
	}
	if err := s.flushTxn(batch); err == nil {
		return nil
	} else if !isConstraintErr(err) {
		return err
	}
	// 约束冲突:逐行落库,FK 失败(书库/系列已删)的行静默跳过
	var firstErr error
	for _, b := range batch {
		if err := s.flushTxn([]BookWrite{b}); err != nil {
			if isConstraintErr(err) {
				continue // 书库/系列已被删除,该行作废
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func isConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "FOREIGN KEY") || strings.Contains(s, "constraint failed")
}

func (s *Store) flushTxn(batch []BookWrite) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	for _, b := range batch {
		if _, err := tx.Exec(`INSERT INTO book(series_id, name, number, file_path, file_size, file_mtime,
  file_last_modified, media_type, media_status, pages_count, pages_json, file_hash, created, last_modified)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(file_path) DO UPDATE SET
  series_id = excluded.series_id, name = excluded.name, number = excluded.number,
  file_size = excluded.file_size, file_mtime = excluded.file_mtime,
  file_last_modified = excluded.file_last_modified, media_type = excluded.media_type,
  media_status = excluded.media_status, pages_count = excluded.pages_count,
  pages_json = excluded.pages_json, file_hash = excluded.file_hash, last_modified = excluded.last_modified`,
			b.Up.SeriesID, b.Up.Name, b.Up.Number, b.Up.FilePath, b.Up.FileSize, b.Up.FileMTime, b.Up.FileLastModified,
			b.Up.MediaType, b.Up.MediaStatus, b.Up.PagesCount, b.Up.PagesJSON, b.Up.FileHash, ts, ts); err != nil {
			return err
		}
		if b.InvalidateThumb {
			if _, err := tx.Exec(`DELETE FROM thumbnail WHERE book_id = (SELECT id FROM book WHERE file_path = ?) AND kind = 'GENERATED'`, b.Up.FilePath); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteMissingBooks(seriesID int64, keepPaths []string) error {
	keep := make(map[string]bool, len(keepPaths))
	for _, p := range keepPaths {
		keep[p] = true
	}
	rows, err := s.db.Query(`SELECT id, file_path FROM book WHERE series_id = ?`, seriesID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		if !keep[p] {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM book WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 阅读进度 ----------

func (s *Store) GetProgress(uid, bookID int64) (*model.ReadProgress, error) {
	var p model.ReadProgress
	var done, created, readDate, lm int64
	err := s.db.QueryRow(
		`SELECT page, completed, created, read_date, last_modified
		 FROM read_progress WHERE user_id = ? AND book_id = ?`,
		uid, bookID).Scan(&p.Page, &done, &created, &readDate, &lm)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.BookID = bookID
	p.Completed = done == 1
	p.Created = time.Unix(created, 0).UTC()
	p.ReadDate = time.Unix(readDate, 0).UTC()
	p.LastModified = time.Unix(lm, 0).UTC()
	return &p, nil
}

func (s *Store) SetProgress(uid, bookID int64, page int, completed bool) (*model.ReadProgress, error) {
	ts := now()
	_, err := s.db.Exec(`INSERT INTO read_progress(user_id, book_id, page, completed, created, read_date, last_modified)
VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(user_id, book_id) DO UPDATE SET
  page = excluded.page, completed = excluded.completed, read_date = excluded.read_date,
  last_modified = excluded.last_modified,
  created = CASE WHEN read_progress.created = 0 THEN excluded.created ELSE read_progress.created END`,
		uid, bookID, page, completed, ts, ts, ts)
	if err != nil {
		return nil, err
	}
	return s.GetProgress(uid, bookID)
}
