package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"komga-lite/internal/api"
	"komga-lite/internal/auth"
	"komga-lite/internal/config"
	"komga-lite/internal/httplog"
	"komga-lite/internal/perf"
	"komga-lite/internal/scanner"
	"komga-lite/internal/store"
	"komga-lite/internal/web"
)

const defaultAdminEmail = "admin@komga.local"
const defaultAdminPassword = "admin123"

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	pf := perf.Detect()
	if cfg.ScanWorkers != nil && *cfg.ScanWorkers > 0 {
		pf.Workers = *cfg.ScanWorkers
		pf.DBConns = clampInt(pf.Workers+1, 2, 8)
		pf.Overridden = true
	}
	pf.Log()
	st, err := store.Open(cfg.Database, pf.DBConns)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()
	a, err := auth.New(st)
	if err != nil {
		log.Fatalf("初始化认证失败: %v", err)
	}

	// 子命令:adduser / apikey / scan
	if args := flag.Args(); len(args) > 0 {
		switch args[0] {
		case "adduser":
			subcmdAddUser(st, args[1:])
		case "apikey":
			subcmdAPIKey(st, args[1:])
		case "scan":
			subcmdScan(st)
		default:
			log.Fatalf("未知子命令 %q,可用: adduser / apikey / scan", args[0])
		}
		return
	}

	// 首次启动创建默认管理员
	if n, err := st.CountUsers(); err != nil {
		log.Fatalf("查询用户失败: %v", err)
	} else if n == 0 {
		hash, _ := auth.HashPassword(defaultAdminPassword)
		if _, err := st.CreateUser(defaultAdminEmail, "管理员", hash, "ADMIN,USER"); err != nil {
			log.Fatalf("创建默认管理员失败: %v", err)
		}
		log.Printf("已创建默认管理员:%s / %s(请尽快用 adduser 换掉)", defaultAdminEmail, defaultAdminPassword)
	}

	// 书库在网页「管理」页维护(数据库持久化),配置文件不再包含书库列表
	sc := scanner.New(st, pf)
	go sc.ScanAll() // 启动时后台扫描,不阻塞服务

	root := http.NewServeMux()
	root.Handle("/api/v1/", a.RequireAPI(api.New(st, sc)))
	root.Handle("/", web.New(st, sc, a))

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	log.Printf("komga-lite 已启动:http://localhost:%d", cfg.Port)
	log.Fatal(http.ListenAndServe(addr, httplog.Middleware(cfg.LogBodiesOn(), root)))
}

// komga-lite adduser 邮箱 密码 [ADMIN]
func subcmdAddUser(st *store.Store, args []string) {
	if len(args) < 2 {
		log.Fatal("用法: komga-lite adduser 邮箱 密码 [ADMIN]")
	}
	email := strings.ToLower(strings.TrimSpace(args[0]))
	roles := "USER"
	if len(args) > 2 && strings.EqualFold(args[2], "ADMIN") {
		roles = "ADMIN,USER"
	}
	hash, err := auth.HashPassword(args[1])
	if err != nil {
		log.Fatal(err)
	}
	id, err := st.CreateUser(email, email, hash, roles)
	if err != nil {
		log.Fatalf("创建用户失败: %v", err)
	}
	log.Printf("用户已创建: %s (id=%d, 角色 %s)", email, id, roles)
}

// komga-lite apikey 邮箱 —— 生成并打印该用户的 API Key
func subcmdAPIKey(st *store.Store, args []string) {
	if len(args) < 1 {
		log.Fatal("用法: komga-lite apikey 邮箱")
	}
	email := strings.ToLower(strings.TrimSpace(args[0]))
	u, err := st.UserByEmail(email)
	if err != nil || u == nil {
		log.Fatalf("用户不存在: %s", email)
	}
	buf := make([]byte, 20)
	rand.Read(buf)
	key := hex.EncodeToString(buf)
	if err := st.SetAPIKey(u.ID, key); err != nil {
		log.Fatalf("写入失败: %v", err)
	}
	log.Printf("API Key(请求头 X-API-Key): %s", key)
}

// komga-lite scan —— 同步扫描所有库后退出
func subcmdScan(st *store.Store) {
	sc := scanner.New(st, perf.Detect())
	sc.ScanAll()
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
