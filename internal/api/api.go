// Package api 实现 Komga /api/v1 兼容层,供 Koharia / Mihon 等客户端连接。
// DTO 字段集对齐 Komga 2.x 真实响应(以官方演示站输出为基准):
// Spring 风格分页(page 从 0 起)、页码从 1 起、mediaType 为 MIME、
// Basic/X-API-Key 认证、进度对象含 created/readDate/deviceId。
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"komga-lite/internal/auth"
	"komga-lite/internal/model"
	"komga-lite/internal/scanner"
	"komga-lite/internal/store"
	"komga-lite/internal/ziputil"
)

const timeFmt = "2006-01-02T15:04:05Z07:00"

type Handler struct {
	st   *store.Store
	scan *scanner.Scanner
}

func New(st *store.Store, sc *scanner.Scanner) http.Handler {
	h := &Handler{st: st, scan: sc}
	m := http.NewServeMux()
	m.HandleFunc("GET /api/v1/libraries", h.listLibraries)
	m.HandleFunc("GET /api/v1/libraries/{id}", h.getLibrary)
	m.HandleFunc("POST /api/v1/libraries/{id}/scan", h.scanLibrary)
	m.HandleFunc("GET /api/v1/series", h.listSeries)
	m.HandleFunc("GET /api/v1/series/{id}", h.getSeries)
	m.HandleFunc("GET /api/v1/series/{id}/books", h.listSeriesBooks)
	m.HandleFunc("GET /api/v1/series/{id}/thumbnail", h.seriesThumbnail)
	m.HandleFunc("GET /api/v1/series/{id}/thumbnails", h.listSeriesThumbnails)
	m.HandleFunc("POST /api/v1/series/{id}/thumbnails", h.uploadSeriesThumbnail)
	m.HandleFunc("GET /api/v1/series/{id}/thumbnails/{tid}", h.getSeriesThumbnailById)
	m.HandleFunc("DELETE /api/v1/series/{id}/thumbnails/{tid}", h.deleteSeriesThumbnail)
	m.HandleFunc("PUT /api/v1/series/{id}/thumbnails/{tid}/selected", h.selectSeriesThumbnail)
	m.HandleFunc("GET /api/v1/books", h.listBooks)
	m.HandleFunc("GET /api/v1/books/{id}", h.getBook)
	m.HandleFunc("GET /api/v1/books/{id}/pages", h.bookPages)
	m.HandleFunc("GET /api/v1/books/{id}/page/{n}", h.bookPage)  // 旧路径(Mihon 等客户端)
	m.HandleFunc("GET /api/v1/books/{id}/pages/{n}", h.bookPage) // Komga 2.x 路径(Koharia)
	m.HandleFunc("GET /api/v1/books/{id}/thumbnail", h.bookThumbnail)
	m.HandleFunc("GET /api/v1/books/{id}/thumbnails", h.listBookThumbnails)
	m.HandleFunc("POST /api/v1/books/{id}/thumbnails", h.uploadBookThumbnail)
	m.HandleFunc("GET /api/v1/books/{id}/thumbnails/{tid}", h.getBookThumbnailById)
	m.HandleFunc("DELETE /api/v1/books/{id}/thumbnails/{tid}", h.deleteBookThumbnail)
	m.HandleFunc("PUT /api/v1/books/{id}/thumbnails/{tid}/selected", h.selectBookThumbnail)
	m.HandleFunc("GET /api/v1/books/{id}/file", h.bookFile)
	m.HandleFunc("GET /api/v1/books/{id}/read-progress", h.getProgress)
	m.HandleFunc("PATCH /api/v1/books/{id}/read-progress", h.patchProgress)
	m.HandleFunc("GET /api/v1/collections", h.listCollections)
	m.HandleFunc("GET /api/v1/readlists", h.listReadlists)
	m.HandleFunc("GET /api/v1/tags", h.listTags)
	m.HandleFunc("GET /api/v1/genres", h.listGenres)
	m.HandleFunc("GET /api/v1/publishers", h.listPublishers)
	m.HandleFunc("GET /api/v1/authors", h.listAuthors)
	m.HandleFunc("GET /api/v1/client-settings/user/list", h.clientSettingsList)
	m.HandleFunc("GET /api/v1/users/me", h.me)
	m.HandleFunc("GET /api/v1/sse/v1/events", h.sseEvents)
	return m
}

// ---------- DTO(对齐 Komga 2.x) ----------

type LibraryDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Root        string `json:"root"`
	Unavailable bool   `json:"unavailable"`
}

type seriesMetadataDTO struct {
	Status               string   `json:"status"`
	StatusLock           bool     `json:"statusLock"`
	Title                string   `json:"title"`
	TitleLock            bool     `json:"titleLock"`
	TitleSort            string   `json:"titleSort"`
	TitleSortLock        bool     `json:"titleSortLock"`
	Summary              string   `json:"summary"`
	SummaryLock          bool     `json:"summaryLock"`
	ReadingDirection     string   `json:"readingDirection"`
	ReadingDirectionLock bool     `json:"readingDirectionLock"`
	Publisher            string   `json:"publisher"`
	PublisherLock        bool     `json:"publisherLock"`
	AgeRating            *int     `json:"ageRating"`
	AgeRatingLock        bool     `json:"ageRatingLock"`
	Language             string   `json:"language"`
	LanguageLock         bool     `json:"languageLock"`
	Genres               []string `json:"genres"`
	GenresLock           bool     `json:"genresLock"`
	Tags                 []string `json:"tags"`
	TagsLock             bool     `json:"tagsLock"`
	TotalBookCount       int      `json:"totalBookCount"`
	TotalBookCountLock   bool     `json:"totalBookCountLock"`
	SharingLabels        []any    `json:"sharingLabels"`
	SharingLabelsLock    bool     `json:"sharingLabelsLock"`
	Links                []any    `json:"links"`
	LinksLock            bool     `json:"linksLock"`
	AlternateTitles      []any    `json:"alternateTitles"`
	AlternateTitlesLock  bool     `json:"alternateTitlesLock"`
	Created              string   `json:"created"`
	LastModified         string   `json:"lastModified"`
}

type booksMetadataDTO struct {
	Authors       []string `json:"authors"`
	Tags          []string `json:"tags"`
	ReleaseDate   *string  `json:"releaseDate"`
	Summary       string   `json:"summary"`
	SummaryNumber string   `json:"summaryNumber"`
	Created       string   `json:"created"`
	LastModified  string   `json:"lastModified"`
}

type SeriesDTO struct {
	ID                   string            `json:"id"`
	LibraryID            string            `json:"libraryId"`
	Name                 string            `json:"name"`
	URL                  string            `json:"url"`
	Created              string            `json:"created"`
	LastModified         string            `json:"lastModified"`
	FileLastModified     string            `json:"fileLastModified"`
	BooksCount           int               `json:"booksCount"`
	BooksReadCount       int               `json:"booksReadCount"`
	BooksUnreadCount     int               `json:"booksUnreadCount"`
	BooksInProgressCount int               `json:"booksInProgressCount"`
	Metadata             seriesMetadataDTO `json:"metadata"`
	BooksMetadata        booksMetadataDTO  `json:"booksMetadata"`
	Deleted              bool              `json:"deleted"`
	Oneshot              bool              `json:"oneshot"`
}

type mediaDTO struct {
	Status               string `json:"status"`
	MediaType            string `json:"mediaType"`
	PagesCount           int    `json:"pagesCount"`
	Comment              string `json:"comment"`
	EpubDivinaCompatible bool   `json:"epubDivinaCompatible"`
	EpubIsKepub          bool   `json:"epubIsKepub"`
	MediaProfile         string `json:"mediaProfile"`
}

type bookMetadataDTO struct {
	Title           string   `json:"title"`
	TitleLock       bool     `json:"titleLock"`
	Summary         string   `json:"summary"`
	SummaryLock     bool     `json:"summaryLock"`
	Number          string   `json:"number"`
	NumberLock      bool     `json:"numberLock"`
	NumberSort      float64  `json:"numberSort"`
	NumberSortLock  bool     `json:"numberSortLock"`
	ReleaseDate     *string  `json:"releaseDate"`
	ReleaseDateLock bool     `json:"releaseDateLock"`
	Authors         []string `json:"authors"`
	AuthorsLock     bool     `json:"authorsLock"`
	Tags            []string `json:"tags"`
	TagsLock        bool     `json:"tagsLock"`
	Isbn            string   `json:"isbn"`
	IsbnLock        bool     `json:"isbnLock"`
	Links           []any    `json:"links"`
	LinksLock       bool     `json:"linksLock"`
	Created         string   `json:"created"`
	LastModified    string   `json:"lastModified"`
}

type readProgressDTO struct {
	BookID       string `json:"bookId"`
	Page         int    `json:"page"`
	Completed    bool   `json:"completed"`
	ReadDate     string `json:"readDate"`
	Created      string `json:"created"`
	LastModified string `json:"lastModified"`
	DeviceID     string `json:"deviceId"`
	DeviceName   string `json:"deviceName"`
}

type BookDTO struct {
	ID               string           `json:"id"`
	SeriesID         string           `json:"seriesId"`
	SeriesTitle      string           `json:"seriesTitle"`
	LibraryID        string           `json:"libraryId"`
	Name             string           `json:"name"`
	URL              string           `json:"url"`
	Number           float64          `json:"number"` // Koharia 为非空 Float,缺省输出 0
	Created          string           `json:"created"`
	LastModified     string           `json:"lastModified"`
	FileLastModified string           `json:"fileLastModified"`
	SizeBytes        int64            `json:"sizeBytes"`
	Size             string           `json:"size"`
	Media            mediaDTO         `json:"media"`
	Metadata         bookMetadataDTO  `json:"metadata"`
	ReadProgress     *readProgressDTO `json:"readProgress"`
	Deleted          bool             `json:"deleted"`
	FileHash         string           `json:"fileHash"`
	Oneshot          bool             `json:"oneshot"`
}

type userDTO struct {
	ID          string          `json:"id"`
	Email       string          `json:"email"`
	Name        string          `json:"name"`
	Roles       []string        `json:"roles"`
	Permissions map[string]bool `json:"permissions"`
	Labels      []string        `json:"labels"`
}

type pageSortDTO struct {
	Empty    bool `json:"empty"`
	Sorted   bool `json:"sorted"`
	Unsorted bool `json:"unsorted"`
}

type pageableDTO struct {
	Offset     int         `json:"offset"`
	PageNumber int         `json:"pageNumber"`
	PageSize   int         `json:"pageSize"`
	Paged      bool        `json:"paged"`
	Unpaged    bool        `json:"unpaged"`
	Sort       pageSortDTO `json:"sort"`
}

// PageDTO 模拟 Spring Data 的分页响应,Komga 客户端按此解析。
type PageDTO struct {
	Content          any         `json:"content"`
	Pageable         pageableDTO `json:"pageable"`
	TotalElements    int64       `json:"totalElements"`
	TotalPages       int         `json:"totalPages"`
	Last             bool        `json:"last"`
	Size             int         `json:"size"`
	Number           int         `json:"number"`
	Sort             pageSortDTO `json:"sort"`
	NumberOfElements int         `json:"numberOfElements"`
	First            bool        `json:"first"`
	Empty            bool        `json:"empty"`
}

// ---------- DTO 转换 ----------

func libraryDTO(l model.Library) LibraryDTO {
	return LibraryDTO{ID: strconv.FormatInt(l.ID, 10), Name: l.Name, Root: l.Root}
}

func seriesDTO(s model.Series) SeriesDTO {
	ts := s.Created.Format(timeFmt)
	ms := s.LastModified.Format(timeFmt)
	return SeriesDTO{
		ID:                   strconv.FormatInt(s.ID, 10),
		LibraryID:            strconv.FormatInt(s.LibraryID, 10),
		Name:                 s.Name,
		URL:                  "",
		Created:              ts,
		LastModified:         ms,
		FileLastModified:     s.FileLastModified.Format(timeFmt),
		BooksCount:           s.BookCount,
		BooksReadCount:       s.ReadCount,
		BooksUnreadCount:     s.UnreadCount(),
		BooksInProgressCount: s.InProgressCount,
		Metadata: seriesMetadataDTO{
			Status: "ENDED", Title: s.Name, TitleSort: s.Name,
			Genres: []string{}, Tags: []string{},
			TotalBookCount: s.BookCount,
			SharingLabels:  []any{}, Links: []any{}, AlternateTitles: []any{},
			Created: ts, LastModified: ms,
		},
		BooksMetadata: booksMetadataDTO{
			Authors: []string{}, Tags: []string{},
			Created: ts, LastModified: ms,
		},
	}
}

func progressDTO(p model.ReadProgress) readProgressDTO {
	return readProgressDTO{
		BookID:       strconv.FormatInt(p.BookID, 10),
		Page:         p.Page,
		Completed:    p.Completed,
		ReadDate:     p.ReadDate.Format(timeFmt),
		Created:      p.Created.Format(timeFmt),
		LastModified: p.LastModified.Format(timeFmt),
	}
}

// mimeMediaType 把库内媒体类型映射为 Komga 2.x 的 MIME 表示。
func mimeMediaType(t model.Book) string {
	if !t.MediaType.Valid {
		return "application/octet-stream"
	}
	switch t.MediaType.String {
	case "CBZ":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

func numberString(v model.Book) string {
	if !v.Number.Valid {
		return "0"
	}
	return strconv.FormatFloat(v.Number.Float64, 'f', -1, 64)
}

func bookDTO(b model.Book) BookDTO {
	number := 0.0
	if b.Number.Valid {
		number = b.Number.Float64
	}
	media := mediaDTO{
		Status:       b.MediaStatus,
		MediaType:    mimeMediaType(b),
		Comment:      "",
		MediaProfile: "DIVINA",
	}
	if b.PagesCount.Valid {
		media.PagesCount = int(b.PagesCount.Int64)
	}
	dto := BookDTO{
		ID:               strconv.FormatInt(b.ID, 10),
		SeriesID:         strconv.FormatInt(b.SeriesID, 10),
		SeriesTitle:      b.SeriesName,
		LibraryID:        strconv.FormatInt(b.LibraryID, 10),
		Name:             b.Name,
		URL:              "/api/v1/books/" + strconv.FormatInt(b.ID, 10),
		Number:           number,
		Created:          b.Created.Format(timeFmt),
		LastModified:     b.LastModified.Format(timeFmt),
		FileLastModified: b.FileLastModified.Format(timeFmt),
		SizeBytes:        b.FileSize,
		Size:             humanSize(b.FileSize),
		Media:            media,
		Metadata: bookMetadataDTO{
			Title: b.Name, Number: numberString(b), NumberSort: number,
			Authors: []string{}, Tags: []string{}, Links: []any{},
			Created: b.Created.Format(timeFmt), LastModified: b.LastModified.Format(timeFmt),
		},
		FileHash: b.FileHash,
	}
	if b.Progress != nil {
		dto.ReadProgress = ptr(progressDTO(*b.Progress))
	}
	return dto
}

func ptr[T any](v T) *T { return &v }

func userDTOFrom(u *model.User) userDTO {
	return userDTO{
		ID: strconv.FormatInt(u.ID, 10), Email: u.Email, Name: u.Name,
		Roles: u.RoleList(),
		Permissions: map[string]bool{
			"MEDIA_READ": true, "FILE_DOWNLOAD": true, "PAGEWISE_READING": true,
		},
		Labels: []string{},
	}
}

// humanSize 输出 Komga 风格的可读大小,如 "79.1 MiB"。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string(rune("KMGTPE"[exp])) + "iB"
}

// ---------- 通用辅助 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	http.Error(w, strconv.Itoa(status)+" "+msg, status)
}

func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

// pageParams 解析 Spring 风格分页与排序参数。
func pageParams(r *http.Request) (page, size int, sorts []store.Sort) {
	q := r.URL.Query()
	page = atoiDefault(q.Get("page"), 0)
	if page < 0 {
		page = 0
	}
	size = atoiDefault(q.Get("size"), 20)
	if size <= 0 {
		size = 20
	}
	if q.Has("unpaged") {
		size = 100000
	}
	for _, sv := range q["sort"] {
		parts := strings.Split(sv, ",")
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		desc := len(parts) > 1 && strings.EqualFold(strings.TrimSpace(parts[1]), "desc")
		sorts = append(sorts, store.Sort{Key: key, Desc: desc})
	}
	return page, size, sorts
}

func writePage(w http.ResponseWriter, page, size, n int, total int64, content any) {
	totalPages := 0
	if size > 0 {
		totalPages = int((total + int64(size) - 1) / int64(size))
	}
	writeJSON(w, 200, PageDTO{
		Content: content,
		Pageable: pageableDTO{
			Offset: page * size, PageNumber: page, PageSize: size,
			Paged: true, Sort: pageSortDTO{Empty: true, Unsorted: true},
		},
		TotalElements: total, TotalPages: totalPages,
		Sort:  pageSortDTO{Empty: true, Unsorted: true},
		First: page == 0, Last: total == 0 || page >= totalPages-1,
		Size: size, Number: page, NumberOfElements: n, Empty: n == 0,
	})
}

// ---------- libraries ----------

func (h *Handler) listLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := h.st.ListLibraries()
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	out := make([]LibraryDTO, 0, len(libs))
	for _, l := range libs {
		out = append(out, libraryDTO(l))
	}
	writeJSON(w, 200, out)
}

func (h *Handler) getLibrary(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	lib, err := h.st.GetLibrary(id)
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	if lib == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, libraryDTO(*lib))
}

func (h *Handler) scanLibrary(w http.ResponseWriter, r *http.Request) {
	if !auth.User(r.Context()).IsAdmin() {
		writeErr(w, 403, "forbidden")
		return
	}
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if lib, err := h.st.GetLibrary(id); err != nil || lib == nil {
		http.NotFound(w, r)
		return
	}
	h.scan.ScanAsync(id)
	w.WriteHeader(http.StatusAccepted)
}

// ---------- series ----------

func (h *Handler) listSeries(w http.ResponseWriter, r *http.Request) {
	u := auth.User(r.Context())
	page, size, sorts := pageParams(r)
	q := store.SeriesQuery{
		UserID: u.ID, Page: page, Size: size, Sorts: sorts,
		Search: strings.TrimSpace(r.URL.Query().Get("search")),
	}
	for _, id := range r.URL.Query()["library_id"] {
		if v, err := strconv.ParseInt(id, 10, 64); err == nil && v > 0 {
			q.LibraryIDs = append(q.LibraryIDs, v)
		}
	}
	q.ReadStatus = r.URL.Query()["read_status"]
	list, total, err := h.st.ListSeries(q)
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	out := make([]SeriesDTO, 0, len(list))
	for _, s := range list {
		out = append(out, seriesDTO(s))
	}
	writePage(w, page, size, len(out), total, out)
}

func (h *Handler) getSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.st.GetSeries(auth.User(r.Context()).ID, id)
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	if s == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, seriesDTO(*s))
}

func (h *Handler) listSeriesBooks(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u := auth.User(r.Context())
	if s, err := h.st.GetSeries(u.ID, id); err != nil || s == nil {
		http.NotFound(w, r)
		return
	}
	page, size, sorts := pageParams(r)
	books, total, err := h.st.ListBooks(u.ID, id, page, size, sorts)
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	out := make([]BookDTO, 0, len(books))
	for _, b := range books {
		out = append(out, bookDTO(b))
	}
	writePage(w, page, size, len(out), total, out)
}

// ensureBookThumb 返回书籍当前封面;生成封面缺失时现场生成并缓存。
func (h *Handler) ensureBookThumb(b *model.Book) ([]byte, string) {
	if data, mt, ok, err := h.st.ThumbGet(b.ID); err == nil && ok {
		return data, mt
	}
	if b.MediaStatus != "READY" || !b.PagesCount.Valid || b.PagesCount.Int64 == 0 {
		return nil, ""
	}
	data, err := scanner.GenerateThumbnail(b.FilePath)
	if err != nil {
		return nil, ""
	}
	if err := h.st.ThumbPutGenerated(b.ID, data); err != nil {
		return nil, ""
	}
	return data, "image/jpeg"
}

func serveThumbBytes(w http.ResponseWriter, data []byte, mediaType string) {
	if mediaType == "" {
		mediaType = "image/jpeg"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

func (h *Handler) serveBookThumb(w http.ResponseWriter, r *http.Request, b *model.Book) {
	data, mt := h.ensureBookThumb(b)
	if len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	serveThumbBytes(w, data, mt)
}

func (h *Handler) bookThumbnail(w http.ResponseWriter, r *http.Request) {
	if b := h.loadBook(w, r); b != nil {
		h.serveBookThumb(w, r, b)
	}
}

func (h *Handler) seriesThumbnail(w http.ResponseWriter, r *http.Request) {
	if b := h.loadSeriesCoverBook(w, r); b != nil {
		h.serveBookThumb(w, r, b)
	}
}

// loadSeriesCoverBook 取系列自定义封面对应的书籍(无自定义时返回第一本可读书,
// 供生成封面端点复用;系列本身无书则写 404 并返回 nil)。
func (h *Handler) loadSeriesCoverBook(w http.ResponseWriter, r *http.Request) *model.Book {
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return nil
	}
	if data, mt, ok, err := h.st.SeriesThumbGet(id); err == nil && ok {
		serveThumbBytes(w, data, mt)
		return nil
	}
	bid, err := h.st.FirstReadyBookID(id)
	if err != nil || bid == 0 {
		http.NotFound(w, r)
		return nil
	}
	b, err := h.st.GetBook(auth.User(r.Context()).ID, bid)
	if err != nil || b == nil {
		http.NotFound(w, r)
		return nil
	}
	return b
}

// ---------- 封面管理(Komga 2.x thumbnails 契约) ----------

type thumbDTO struct {
	ID        string `json:"id"`
	BookID    string `json:"bookId,omitempty"`
	SeriesID  string `json:"seriesId,omitempty"`
	Type      string `json:"type"`
	Selected  bool   `json:"selected"`
	MediaType string `json:"mediaType"`
	FileSize  int    `json:"fileSize"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

func thumbDims(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func parseThumbKind(tid string) string {
	if strings.HasSuffix(tid, "-USER") {
		return "USER"
	}
	if strings.HasSuffix(tid, "-GENERATED") {
		return "GENERATED"
	}
	return ""
}

// listBookThumbnails GET /books/{id}/thumbnails —— 生成封面不存在时现场补齐,
// 保证列表中始终有 GENERATED 项(Koharia 设置封面依赖此列表)。
func (h *Handler) listBookThumbnails(w http.ResponseWriter, r *http.Request) {
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	h.ensureBookThumb(b)
	out := []thumbDTO{}
	userData, userMT, hasUser, _ := h.st.ThumbGetKind(b.ID, "USER")
	if hasUser {
		tw, th := thumbDims(userData)
		out = append(out, thumbDTO{
			ID: fmt.Sprintf("%d-USER", b.ID), BookID: strconv.FormatInt(b.ID, 10),
			Type: "USER_UPLOADED", Selected: true, MediaType: userMT,
			FileSize: len(userData), Width: tw, Height: th,
		})
	}
	genData, genMT, hasGen, _ := h.st.ThumbGetKind(b.ID, "GENERATED")
	if hasGen {
		tw, th := thumbDims(genData)
		out = append(out, thumbDTO{
			ID: fmt.Sprintf("%d-GENERATED", b.ID), BookID: strconv.FormatInt(b.ID, 10),
			Type: "GENERATED", Selected: !hasUser, MediaType: genMT,
			FileSize: len(genData), Width: tw, Height: th,
		})
	}
	writeJSON(w, 200, out)
}

// readUpload 读取 multipart 字段 file(封面上传),校验为图片,限制 20MB。
func readUpload(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "invalid multipart body")
		return nil, "", false
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "missing file field")
		return nil, "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 20<<20))
	if err != nil || len(data) == 0 {
		writeErr(w, 400, "empty file")
		return nil, "", false
	}
	ct := http.DetectContentType(data[:min(512, len(data))])
	if !strings.HasPrefix(ct, "image/") {
		writeErr(w, 400, "not an image")
		return nil, "", false
	}
	return data, ct, true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// uploadBookThumbnail POST /books/{id}/thumbnails —— 上传自定义封面(原样保存)。
func (h *Handler) uploadBookThumbnail(w http.ResponseWriter, r *http.Request) {
	if !auth.User(r.Context()).IsAdmin() {
		writeErr(w, 403, "forbidden")
		return
	}
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	data, ct, ok := readUpload(w, r)
	if !ok {
		return
	}
	if err := h.st.ThumbPutUser(b.ID, data, ct); err != nil {
		writeErr(w, 500, "save failed")
		return
	}
	h.listBookThumbnails(w, r) // 返回更新后的列表
}

// getBookThumbnailById GET /books/{id}/thumbnails/{tid}
func (h *Handler) getBookThumbnailById(w http.ResponseWriter, r *http.Request) {
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	kind := parseThumbKind(r.PathValue("tid"))
	if kind == "" {
		http.NotFound(w, r)
		return
	}
	if kind == "GENERATED" {
		h.serveBookThumb(w, r, b) // 懒生成
		return
	}
	data, mt, ok, _ := h.st.ThumbGetKind(b.ID, kind)
	if !ok {
		http.NotFound(w, r)
		return
	}
	serveThumbBytes(w, data, mt)
}

// deleteBookThumbnail DELETE /books/{id}/thumbnails/{tid} —— 仅允许删除用户封面。
func (h *Handler) deleteBookThumbnail(w http.ResponseWriter, r *http.Request) {
	if !auth.User(r.Context()).IsAdmin() {
		writeErr(w, 403, "forbidden")
		return
	}
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	if parseThumbKind(r.PathValue("tid")) != "USER" {
		writeErr(w, 400, "only user-uploaded thumbnails can be deleted")
		return
	}
	if ok, err := h.st.ThumbDeleteUser(b.ID); err != nil {
		writeErr(w, 500, "delete failed")
		return
	} else if !ok {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// selectBookThumbnail PUT /books/{id}/thumbnails/{tid}/selected
// 选中 USER = 已是默认行为;选中 GENERATED = 移除用户封面(恢复默认)。
func (h *Handler) selectBookThumbnail(w http.ResponseWriter, r *http.Request) {
	if !auth.User(r.Context()).IsAdmin() {
		writeErr(w, 403, "forbidden")
		return
	}
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	switch parseThumbKind(r.PathValue("tid")) {
	case "USER":
		if _, _, ok, _ := h.st.ThumbGetKind(b.ID, "USER"); !ok {
			http.NotFound(w, r)
			return
		}
	case "GENERATED":
		h.st.ThumbDeleteUser(b.ID) // 恢复默认封面
	default:
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ---------- 系列封面 ----------

func (h *Handler) listSeriesThumbnails(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	out := []thumbDTO{}
	if data, mt, has, err := h.st.SeriesThumbGet(id); err == nil && has {
		tw, th := thumbDims(data)
		out = append(out, thumbDTO{
			ID: fmt.Sprintf("%d-USER", id), SeriesID: strconv.FormatInt(id, 10),
			Type: "USER_UPLOADED", Selected: true, MediaType: mt,
			FileSize: len(data), Width: tw, Height: th,
		})
	}
	// GENERATED 项:取系列首本可读书的封面元信息
	if bid, err := h.st.FirstReadyBookID(id); err == nil && bid > 0 {
		if b, err := h.st.GetBook(auth.User(r.Context()).ID, bid); err == nil && b != nil {
			if data, mt := h.ensureBookThumb(b); len(data) > 0 {
				tw, th := thumbDims(data)
				out = append(out, thumbDTO{
					ID: fmt.Sprintf("%d-GENERATED", id), SeriesID: strconv.FormatInt(id, 10),
					Type: "GENERATED", Selected: len(out) == 0, MediaType: mt,
					FileSize: len(data), Width: tw, Height: th,
				})
			}
		}
	}
	writeJSON(w, 200, out)
}

func (h *Handler) uploadSeriesThumbnail(w http.ResponseWriter, r *http.Request) {
	if !auth.User(r.Context()).IsAdmin() {
		writeErr(w, 403, "forbidden")
		return
	}
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, ct, ok2 := readUpload(w, r)
	if !ok2 {
		return
	}
	if err := h.st.SeriesThumbPut(id, data, ct); err != nil {
		writeErr(w, 500, "save failed")
		return
	}
	h.listSeriesThumbnails(w, r)
}

func (h *Handler) getSeriesThumbnailById(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if parseThumbKind(r.PathValue("tid")) == "USER" {
		if data, mt, has, err := h.st.SeriesThumbGet(id); err == nil && has {
			serveThumbBytes(w, data, mt)
			return
		}
		http.NotFound(w, r)
		return
	}
	h.seriesThumbnail(w, r) // GENERATED:走默认系列封面
}

func (h *Handler) deleteSeriesThumbnail(w http.ResponseWriter, r *http.Request) {
	if !auth.User(r.Context()).IsAdmin() {
		writeErr(w, 403, "forbidden")
		return
	}
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if parseThumbKind(r.PathValue("tid")) != "USER" {
		writeErr(w, 400, "only user-uploaded thumbnails can be deleted")
		return
	}
	if has, err := h.st.SeriesThumbDelete(id); err != nil {
		writeErr(w, 500, "delete failed")
		return
	} else if !has {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) selectSeriesThumbnail(w http.ResponseWriter, r *http.Request) {
	if !auth.User(r.Context()).IsAdmin() {
		writeErr(w, 403, "forbidden")
		return
	}
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch parseThumbKind(r.PathValue("tid")) {
	case "USER":
		if _, _, has, _ := h.st.SeriesThumbGet(id); !has {
			http.NotFound(w, r)
			return
		}
	case "GENERATED":
		h.st.SeriesThumbDelete(id)
	default:
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ---------- books ----------

// listBooks 全量书列表:Koharia/Mihon 等客户端用它做全库同步。
// 支持 library_id / series_id / search / read_status / deleted=false / 分页排序。
func (h *Handler) listBooks(w http.ResponseWriter, r *http.Request) {
	u := auth.User(r.Context())
	page, size, sorts := pageParams(r)
	q := store.BookQuery{
		UserID: u.ID, Page: page, Size: size, Sorts: sorts,
		Search: strings.TrimSpace(r.URL.Query().Get("search")),
	}
	for _, id := range r.URL.Query()["library_id"] {
		if v, err := strconv.ParseInt(id, 10, 64); err == nil && v > 0 {
			q.LibraryIDs = append(q.LibraryIDs, v)
		}
	}
	for _, id := range r.URL.Query()["series_id"] {
		if v, err := strconv.ParseInt(id, 10, 64); err == nil && v > 0 {
			q.SeriesIDs = append(q.SeriesIDs, v)
		}
	}
	q.ReadStatus = r.URL.Query()["read_status"]
	books, total, err := h.st.ListAllBooks(q)
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	out := make([]BookDTO, 0, len(books))
	for _, b := range books {
		out = append(out, bookDTO(b))
	}
	writePage(w, page, size, len(out), total, out)
}

func (h *Handler) loadBook(w http.ResponseWriter, r *http.Request) *model.Book {
	id, ok := idParam(r)
	if !ok {
		http.NotFound(w, r)
		return nil
	}
	b, err := h.st.GetBook(auth.User(r.Context()).ID, id)
	if err != nil {
		writeErr(w, 500, "internal error")
		return nil
	}
	if b == nil {
		http.NotFound(w, r)
		return nil
	}
	return b
}

func (h *Handler) getBook(w http.ResponseWriter, r *http.Request) {
	if b := h.loadBook(w, r); b != nil {
		writeJSON(w, 200, bookDTO(*b))
	}
}

func (h *Handler) bookPage(w http.ResponseWriter, r *http.Request) {
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || !b.PagesCount.Valid || n > int(b.PagesCount.Int64) {
		writeErr(w, 404, "page not found")
		return
	}
	data, ct, err := ziputil.ReadPage(b.FilePath, n-1)
	if err != nil {
		writeErr(w, 404, "page not found")
		return
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

type pageDTO struct {
	Number    int    `json:"number"`
	FileName  string `json:"fileName"`
	MediaType string `json:"mediaType"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	SizeBytes int64  `json:"sizeBytes"`
	Size      string `json:"size"`
}

func (h *Handler) bookPages(w http.ResponseWriter, r *http.Request) {
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	var pages []ziputil.Page
	if b.PagesJSON.Valid {
		json.Unmarshal([]byte(b.PagesJSON.String), &pages)
	}
	out := make([]pageDTO, 0, len(pages))
	for i, p := range pages {
		out = append(out, pageDTO{
			Number: i + 1, FileName: p.Name, MediaType: p.ContentType,
			Width: p.Width, Height: p.Height,
			SizeBytes: p.Size, Size: humanSize(p.Size),
		})
	}
	writeJSON(w, 200, out)
}

func (h *Handler) bookFile(w http.ResponseWriter, r *http.Request) {
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	f, err := os.Open(b.FilePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	name := b.Name + ".zip"
	w.Header().Set("Content-Disposition",
		`attachment; filename="book-`+strconv.FormatInt(b.ID, 10)+`.zip"; filename*=UTF-8''`+url.PathEscape(name))
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// ---------- read progress ----------

// getProgress 为兼容旧客户端保留;Komga 2.x 本身已改为通过 BookDto.readProgress 读取。
func (h *Handler) getProgress(w http.ResponseWriter, r *http.Request) {
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	p, err := h.st.GetProgress(auth.User(r.Context()).ID, b.ID)
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	if p == nil {
		http.NotFound(w, r) // Komga: 无进度返回 404
		return
	}
	writeJSON(w, 200, progressDTO(*p))
}

func (h *Handler) patchProgress(w http.ResponseWriter, r *http.Request) {
	b := h.loadBook(w, r)
	if b == nil {
		return
	}
	var body struct {
		Page      *int  `json:"page"`
		Completed *bool `json:"completed"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.Page == nil {
		writeErr(w, 400, "invalid body")
		return
	}
	page := *body.Page
	if page < 1 || (b.PagesCount.Valid && page > int(b.PagesCount.Int64)) {
		writeErr(w, 400, "page out of range")
		return
	}
	completed := body.Completed != nil && *body.Completed
	if b.PagesCount.Valid && page >= int(b.PagesCount.Int64) {
		completed = true
	}
	p, err := h.st.SetProgress(auth.User(r.Context()).ID, b.ID, page, completed)
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	writeJSON(w, 200, progressDTO(*p))
}

// ---------- users ----------

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, userDTOFrom(auth.User(r.Context())))
}

// ---------- 合集 / 书单 / 参照表 / 客户端设置(暂为空实现) ----------

func (h *Handler) listCollections(w http.ResponseWriter, r *http.Request) {
	page, size, _ := pageParams(r)
	writePage(w, page, size, 0, 0, []any{})
}

func (h *Handler) listReadlists(w http.ResponseWriter, r *http.Request) {
	page, size, _ := pageParams(r)
	writePage(w, page, size, 0, 0, []any{})
}

func (h *Handler) listTags(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, []string{})
}

func (h *Handler) listGenres(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, []string{})
}

func (h *Handler) listPublishers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, []string{})
}

// authorDTO 对应 Komga 2.x /api/v1/authors 的元素形状 {name, role}。
type authorDTO struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

func (h *Handler) listAuthors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, []authorDTO{})
}

// clientSettingsList 返回空的 per-client 设置映射,客户端会用自身默认值。
func (h *Handler) clientSettingsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{})
}

// sseEvents 事件流占位:保持连接并定期发心跳注释。
// 官方 Komga 2.x 对此路径同样返回 404,Koharia 把它当可选依赖,
// 挂住连接即可避免客户端反复重连。
func (h *Handler) sseEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}
