// dsncheck 验证 modernc.org/sqlite 的 _pragma DSN 参数是否真正生效
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	dir, _ := os.MkdirTemp("", "dsn")
	defer os.RemoveAll(dir)

	forms := map[string]string{
		"裸路径+参数":  dir + "/a.db?_pragma=busy_timeout(12345)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		"file前缀+参数": "file:" + dir + "/b.db?_pragma=busy_timeout(12345)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
	}
	for name, dsn := range forms {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			fmt.Printf("%-10s 打开失败: %v\n", name, err)
			continue
		}
		db.SetMaxOpenConns(4)
		var jm, bt, fk string
		err1 := db.QueryRow(`PRAGMA journal_mode`).Scan(&jm)
		if _, e := db.Exec(`CREATE TABLE t(x)`); e != nil {
			fmt.Printf("%-10s 建表失败: %v\n", name, e)
		}
		err2 := db.QueryRow(`PRAGMA busy_timeout`).Scan(&bt)
		err3 := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
		fmt.Printf("%-10s journal=%s busy=%s fk=%s (err %v %v %v)\n", name, jm, bt, fk, err1, err2, err3)
		db.Close()
	}
}
