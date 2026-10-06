// Package httplog 输出每个请求的方法、路径、状态、耗时,
// 以及文本类请求体(用于调试客户端交互)。
// 不记录响应体;二进制请求体同样跳过,登录密码自动打码。
package httplog

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxBody 单个请求体最多记录的字节数。
const maxBody = 8 << 10

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (c *statusWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *statusWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// textual 判断是否为值得记录的文本内容。
func textual(ct string) bool {
	ct = strings.ToLower(ct)
	return ct == "" ||
		strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "x-www-form-urlencoded") ||
		strings.Contains(ct, "xml") ||
		strings.Contains(ct, "opds")
}

// mask 登录表单中的明文密码。
func mask(path, method string, body []byte) string {
	if strings.HasSuffix(path, "/login") && len(body) > 0 {
		if v, err := url.ParseQuery(string(body)); err == nil && v.Get("password") != "" {
			v.Set("password", "***")
			return v.Encode()
		}
	}
	return string(body)
}

func trunc(b []byte, total int64) string {
	s := string(b)
	s = strings.Map(func(r rune) rune { // 去掉控制字符,防止刷屏
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 32 {
			return -1
		}
		return r
	}, s)
	if total > int64(len(b)) {
		s += " ...(已截断, 共 " + itoa(total) + " 字节)"
	}
	return s
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Middleware 记录请求;logBodies 为 false 时只记录请求行。
func Middleware(logBodies bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		var reqBody []byte
		var reqTotal int64
		if logBodies && r.Body != nil && textual(r.Header.Get("Content-Type")) {
			data, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
			reqBody = data
			reqTotal = r.ContentLength
			if reqTotal < 0 {
				reqTotal = int64(len(data))
			}
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data), r.Body))
		}

		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)

		dur := time.Since(start).Round(time.Millisecond)
		log.Printf("[http] %s %s %s -> %d (%v)", clientIP(r), r.Method, r.URL.RequestURI(), sw.status, dur)
		if logBodies && len(reqBody) > 0 {
			log.Printf("[http]   请求: %s", trunc([]byte(mask(r.URL.Path, r.Method, reqBody)), reqTotal))
		}
	})
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
