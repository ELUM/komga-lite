// Package auth 提供 Komga 客户端所需的认证方式:
// API 走 X-API-Key 或 HTTP Basic,网页走 HMAC 签名会话 Cookie。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"komga-lite/internal/model"
	"komga-lite/internal/store"
)

const CookieName = "KL_SESSION"

const sessionTTL = 30 * 24 * time.Hour

type Auth struct {
	st     *store.Store
	secret []byte
}

func New(st *store.Store) (*Auth, error) {
	sec, err := st.Secret()
	if err != nil {
		return nil, err
	}
	return &Auth{st: st, secret: sec}, nil
}

func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

func (a *Auth) Verify(email, password string) *model.User {
	u, err := a.st.UserByEmail(strings.ToLower(strings.TrimSpace(email)))
	if u == nil || err != nil {
		return nil
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil
	}
	return u
}

// ---- 会话 Cookie 格式: "uid|expiryUnix|hex(hmac-sha256)" ----

func (a *Auth) sign(payload string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

func (a *Auth) IssueSession(uid int64) *http.Cookie {
	exp := time.Now().Add(sessionTTL).Unix()
	payload := fmt.Sprintf("%d|%d", uid, exp)
	return &http.Cookie{
		Name:     CookieName,
		Value:    payload + "|" + a.sign(payload),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(exp, 0),
	}
}

func (a *Auth) ClearSession() *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1}
}

func (a *Auth) userFromCookie(r *http.Request) *model.User {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return nil
	}
	payload := parts[0] + "|" + parts[1]
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(a.sign(payload))) != 1 {
		return nil
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || exp < time.Now().Unix() {
		return nil
	}
	u, err := a.st.UserByID(uid)
	if u == nil || err != nil {
		return nil
	}
	return u
}

// UserFromRequest 按优先级解析调用者:X-API-Key > Basic > 会话 Cookie。
func (a *Auth) UserFromRequest(r *http.Request) *model.User {
	if k := r.Header.Get("X-API-Key"); k != "" {
		if u, err := a.st.UserByAPIKey(k); u != nil && err == nil {
			return u
		}
		return nil
	}
	if email, pw, ok := r.BasicAuth(); ok {
		return a.Verify(email, pw)
	}
	return a.userFromCookie(r)
}

type ctxKey struct{}

// RequireAPI 守卫 /api/v1/**,与 Komga 客户端约定一致。
func (a *Auth) RequireAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := a.UserFromRequest(r)
		if u == nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="komga-lite"`)
			http.Error(w, "401 unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

// RequireSession 守卫网页,未登录跳转 /login。
func (a *Auth) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := a.userFromCookie(r)
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func User(ctx context.Context) *model.User {
	u, _ := ctx.Value(ctxKey{}).(*model.User)
	return u
}
