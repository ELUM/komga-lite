// Package ziputil 负责从 zip/cbz 压缩包中按阅读顺序提取图片页。
package ziputil

import (
	"archive/zip"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// Page 是压缩包内一页的元信息;json 标签同时用于数据库 pages_json 列。
type Page struct {
	Name        string `json:"name"`
	Width       int    `json:"w"`
	Height      int    `json:"h"`
	Size        int64  `json:"s"`
	ContentType string `json:"t"`
}

var imageExts = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
}

// ImageType 依据扩展名返回 MIME 类型;非图片返回空串。
func ImageType(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return imageExts[strings.ToLower(name[i:])]
}

// fixName 修复以 GBK 编码存储的 zip 条目名(常见于国产工具打包的压缩包)。
func fixName(n string) string {
	n = strings.ReplaceAll(n, "\\", "/")
	if !utf8.ValidString(n) {
		if dec, err := simplifiedchinese.GBK.NewDecoder().String(n); err == nil {
			n = dec
		}
	}
	return n
}

func openZip(path string) (*os.File, *zip.Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, zr, nil
}

// pageEntries 收集压缩包内全部图片条目(递归包含子目录),同名取先出现者。
func pageEntries(zr *zip.Reader) map[string]*zip.File {
	m := make(map[string]*zip.File)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		n := fixName(f.Name)
		if ImageType(n) == "" {
			continue
		}
		if _, dup := m[n]; !dup {
			m[n] = f
		}
	}
	return m
}

func sortedNames(m map[string]*zip.File) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return NatLess(names[i], names[j]) })
	return names
}

// ListPages 返回压缩包内图片页列表,按自然阅读顺序排列。
func ListPages(path string) ([]Page, error) {
	f, zr, err := openZip(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := pageEntries(zr)
	names := sortedNames(m)
	pages := make([]Page, 0, len(names))
	for _, n := range names {
		p := Page{Name: n, ContentType: ImageType(n), Size: int64(m[n].UncompressedSize64)}
		if rc, err := m[n].Open(); err == nil {
			if cfg, _, err := image.DecodeConfig(rc); err == nil {
				p.Width, p.Height = cfg.Width, cfg.Height
			}
			rc.Close()
		}
		pages = append(pages, p)
	}
	return pages, nil
}

// ReadPage 返回第 index 页(0 起)的原始字节及其 MIME 类型。
func ReadPage(path string, index int) ([]byte, string, error) {
	f, zr, err := openZip(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	m := pageEntries(zr)
	names := sortedNames(m)
	if index < 0 || index >= len(names) {
		return nil, "", os.ErrNotExist
	}
	rc, err := m[names[index]].Open()
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", err
	}
	return data, ImageType(names[index]), nil
}

// NatLess 自然序比较:数字段按数值比较,使 page_2 排在 page_10 之前。
func NatLess(a, b string) bool {
	ra, rb := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if isDigit(ra[i]) && isDigit(rb[j]) {
			si, sj := i, j
			for i < len(ra) && isDigit(ra[i]) {
				i++
			}
			for j < len(rb) && isDigit(rb[j]) {
				j++
			}
			na := strings.TrimLeft(string(ra[si:i]), "0")
			nb := strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
		} else {
			if ra[i] != rb[j] {
				return ra[i] < rb[j]
			}
			i++
			j++
		}
	}
	return len(ra)-i < len(rb)-j
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
