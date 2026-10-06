package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"komga-lite/internal/auth"
)

func parseID(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

// requireAdmin 管理书库的页面与 API 仅管理员可用。
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !auth.User(r.Context()).IsAdmin() {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return false
	}
	return true
}

type fsEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type fsResp struct {
	Path   string    `json:"path"`
	Parent string    `json:"parent"`
	Dirs   []fsEntry `json:"dirs"`
}

// adminFS 列出服务器上某目录的子目录(只列目录),path 为空时返回盘符/根。
func (h *Handler) adminFS(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	path := r.URL.Query().Get("path")
	resp := fsResp{Dirs: []fsEntry{}}

	if path == "" {
		if runtime.GOOS == "windows" {
			for c := 'A'; c <= 'Z'; c++ {
				d := string(c) + `:\`
				if _, err := os.Stat(d); err == nil {
					resp.Dirs = append(resp.Dirs, fsEntry{Name: d, Path: d})
				}
			}
		} else {
			resp.Path, resp.Parent = "/", "/"
			h.fillDirs(&resp, "/")
		}
		writeJSONAdmin(w, resp)
		return
	}

	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		http.Error(w, `{"error":"目录不存在"}`, http.StatusBadRequest)
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	resp.Path = abs
	if parent := filepath.Dir(abs); parent != abs {
		resp.Parent = parent
	}
	h.fillDirs(&resp, abs)
	writeJSONAdmin(w, resp)
}

func (h *Handler) fillDirs(resp *fsResp, path string) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	for _, n := range names {
		resp.Dirs = append(resp.Dirs, fsEntry{Name: n, Path: filepath.Join(path, n)})
	}
}

func writeJSONAdmin(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// adminCreateLibrary 创建书库并立即触发扫描。
func (h *Handler) adminCreateLibrary(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct {
		Name string `json:"name"`
		Root string `json:"root"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Root) == "" {
		writeJSONAdmin(w, map[string]any{"error": "请填写路径"})
		return
	}
	root, err := filepath.Abs(strings.TrimSpace(body.Root))
	if err != nil {
		writeJSONAdmin(w, map[string]any{"error": "路径无效"})
		return
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		writeJSONAdmin(w, map[string]any{"error": "目录不存在: " + root})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = filepath.Base(root)
	}
	id, err := h.st.UpsertLibrary(name, root)
	if err != nil {
		writeJSONAdmin(w, map[string]any{"error": err.Error()})
		return
	}
	h.scan.ScanAsync(id)
	writeJSONAdmin(w, map[string]any{"id": id})
}

// adminDeleteLibrary 删除书库(级联删除其系列/书籍/进度)。
func (h *Handler) adminDeleteLibrary(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.st.DeleteLibrary(id); err != nil {
		writeJSONAdmin(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSONAdmin(w, map[string]any{"ok": true})
}

// adminUpdateLibrary 编辑书库(名称/根目录);根目录变化后自动重扫。
func (h *Handler) adminUpdateLibrary(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Name string `json:"name"`
		Root string `json:"root"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Root) == "" {
		writeJSONAdmin(w, map[string]any{"error": "请填写路径"})
		return
	}
	root, err := filepath.Abs(strings.TrimSpace(body.Root))
	if err != nil {
		writeJSONAdmin(w, map[string]any{"error": "路径无效"})
		return
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		writeJSONAdmin(w, map[string]any{"error": "目录不存在: " + root})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = filepath.Base(root)
	}
	old, err := h.st.GetLibrary(id)
	if err != nil || old == nil {
		http.NotFound(w, r)
		return
	}
	if err := h.st.UpdateLibrary(id, name, root); err != nil {
		writeJSONAdmin(w, map[string]any{"error": "保存失败(路径是否已被其他书库使用?): " + err.Error()})
		return
	}
	if old.Root != root {
		h.scan.ScanAsync(id) // 根目录变了,重扫清理旧数据并入库新内容
	}
	writeJSONAdmin(w, map[string]any{"ok": true})
}
