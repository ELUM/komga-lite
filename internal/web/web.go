// Package web 提供 html/template 服务端渲染页面:
// 登录、库/系列浏览、简易阅读器(进度与 API 共享,客户端 App 可同步)。
package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"

	"komga-lite/internal/auth"
	"komga-lite/internal/model"
	"komga-lite/internal/scanner"
	"komga-lite/internal/store"
)

//go:embed templates/*.html
var tmplFS embed.FS

type Handler struct {
	st   *store.Store
	scan *scanner.Scanner
	auth *auth.Auth
	t    *template.Template
}

func New(st *store.Store, sc *scanner.Scanner, a *auth.Auth) http.Handler {
	h := &Handler{st: st, scan: sc, auth: a}
	h.t = template.Must(template.New("").Funcs(template.FuncMap{
		"progressLabel": progressLabel,
	}).ParseFS(tmplFS, "templates/*.html"))
	m := http.NewServeMux()
	m.HandleFunc("GET /login", h.loginPage)
	m.HandleFunc("POST /login", h.loginSubmit)
	m.HandleFunc("GET /logout", h.logout)
	m.Handle("GET /{$}", a.RequireSession(http.HandlerFunc(h.home)))
	m.Handle("GET /libraries/{id}", a.RequireSession(http.HandlerFunc(h.libraryPage)))
	m.Handle("GET /series/{id}", a.RequireSession(http.HandlerFunc(h.seriesPage)))
	m.Handle("GET /books/{id}/read", a.RequireSession(http.HandlerFunc(h.readerPage)))
	// 书库管理 API(主页卡片调用,管理员)
	m.Handle("GET /admin/api/fs", a.RequireSession(http.HandlerFunc(h.adminFS)))
	m.Handle("POST /admin/api/libraries", a.RequireSession(http.HandlerFunc(h.adminCreateLibrary)))
	m.Handle("PUT /admin/api/libraries/{id}", a.RequireSession(http.HandlerFunc(h.adminUpdateLibrary)))
	m.Handle("DELETE /admin/api/libraries/{id}", a.RequireSession(http.HandlerFunc(h.adminDeleteLibrary)))
	return m
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "模板渲染失败: "+err.Error(), http.StatusInternalServerError)
	}
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if h.auth.UserFromRequest(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.render(w, "login.html", map[string]any{"Error": ""})
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u := h.auth.Verify(r.FormValue("email"), r.FormValue("password"))
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		h.render(w, "login.html", map[string]any{"Error": "邮箱或密码错误"})
		return
	}
	http.SetCookie(w, h.auth.IssueSession(u.ID))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, h.auth.ClearSession())
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	u := auth.User(r.Context())
	libs, err := h.st.ListLibraries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 供前端编辑弹窗预填的 JSON(id/name/root)
	type libJ struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Root string `json:"root"`
	}
	arr := make([]libJ, 0, len(libs))
	for _, l := range libs {
		arr = append(arr, libJ{l.ID, l.Name, l.Root})
	}
	b, _ := json.Marshal(arr)
	h.render(w, "index.html", map[string]any{
		"Libraries": libs,
		"IsAdmin":   u.IsAdmin(),
		"Libs":      template.JS(b),
	})
}

func (h *Handler) libraryPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	lib, err := h.st.GetLibrary(id)
	if err != nil || lib == nil {
		http.NotFound(w, r)
		return
	}
	uid := auth.User(r.Context()).ID
	series, _, err := h.st.ListSeries(store.SeriesQuery{UserID: uid, LibraryIDs: []int64{id}, Page: 0, Size: 1000})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, "library.html", map[string]any{"Lib": lib, "Series": series})
}

func (h *Handler) seriesPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	uid := auth.User(r.Context()).ID
	s, err := h.st.GetSeries(uid, id)
	if err != nil || s == nil {
		http.NotFound(w, r)
		return
	}
	books, _, err := h.st.ListBooks(uid, id, 0, 1000, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, "series.html", map[string]any{"Series": s, "Books": books})
}

func (h *Handler) readerPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, err := h.st.GetBook(auth.User(r.Context()).ID, id)
	if err != nil || b == nil {
		http.NotFound(w, r)
		return
	}
	total := 0
	if b.PagesCount.Valid {
		total = int(b.PagesCount.Int64)
	}
	if total == 0 || b.MediaStatus != "READY" {
		http.Error(w, "此书无可读页面", http.StatusNotFound)
		return
	}
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v >= 1 && v <= total {
			page = v
		}
	} else if b.Progress != nil && b.Progress.Page < total {
		page = b.Progress.Page + 1 // 从上次进度继续
	}
	prev, next := 0, 0
	if page > 1 {
		prev = page - 1
	}
	if page < total {
		next = page + 1
	}
	h.render(w, "read.html", map[string]any{
		"Book": b, "Page": page, "Total": total, "Prev": prev, "Next": next,
		"IsAdmin": auth.User(r.Context()).IsAdmin(),
	})
}

// 供模板使用的进度展示辅助
func progressLabel(b model.Book) string {
	if b.Progress == nil {
		return "未读"
	}
	if b.Progress.Completed {
		return "已读完"
	}
	return "读到第 " + strconv.Itoa(b.Progress.Page) + " 页"
}
