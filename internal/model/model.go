package model

import (
	"database/sql"
	"strings"
	"time"
)

type User struct {
	ID           int64
	Email        string
	Name         string
	PasswordHash string
	Roles        string // 逗号分隔,如 "ADMIN,USER"
	APIKey       string // 未设置为空
}

func (u *User) IsAdmin() bool {
	for _, r := range strings.Split(u.Roles, ",") {
		if strings.TrimSpace(strings.ToUpper(r)) == "ADMIN" {
			return true
		}
	}
	return false
}

func (u *User) RoleList() []string {
	var out []string
	for _, r := range strings.Split(u.Roles, ",") {
		if r = strings.TrimSpace(strings.ToUpper(r)); r != "" {
			out = append(out, r)
		}
	}
	return out
}

type Library struct {
	ID   int64
	Name string
	Root string
}

type Series struct {
	ID               int64
	LibraryID        int64
	Name             string
	Created          time.Time
	LastModified     time.Time
	FileLastModified time.Time
	BookCount        int
	ReadCount        int
	InProgressCount  int
}

func (s *Series) UnreadCount() int {
	n := s.BookCount - s.ReadCount - s.InProgressCount
	if n < 0 {
		n = 0
	}
	return n
}

type Book struct {
	ID               int64
	SeriesID         int64
	LibraryID        int64
	SeriesName       string
	Name             string
	Number           sql.NullFloat64
	FilePath         string
	FileSize         int64
	FileLastModified time.Time
	MediaType        sql.NullString
	MediaStatus      string // READY / ERROR / UNKNOWN / UNSUPPORTED
	PagesCount       sql.NullInt64
	PagesJSON        sql.NullString
	FileHash         string
	Created          time.Time
	LastModified     time.Time
	Progress         *ReadProgress // 当前用户进度,可为 nil
}

type ReadProgress struct {
	BookID       int64
	Page         int
	Completed    bool
	Created      time.Time
	ReadDate     time.Time
	LastModified time.Time
}
