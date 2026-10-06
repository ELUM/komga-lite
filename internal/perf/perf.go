// Package perf 启动时探测 CPU 与内存,决定扫描并发档位。
// 探测一次性完成(基准测试约 150ms),不依赖外部库。
package perf

import (
	"bufio"
	"crypto/sha256"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Perf 是探测结果与由此得出的并发档位。
type Perf struct {
	NumCPU       int
	MemAvailMB   int // -1 表示未知(非 Linux 或读取失败)
	HashMBPerSec float64

	Workers    int  // 单库扫描 worker 数
	LibConc    int  // 同时扫描的书库数
	Prewarm    int  // 封面预热并发
	DBConns    int  // SQLite 连接池大小
	Overridden bool // 手动覆盖过
}

// 慢 CPU 阈值:单核 SHA-256 吞吐低于此值视为慢核,worker 减半。
const slowThresholdMBps = 150.0

// Detect 探测本机性能并计算并发档位。
func Detect() Perf {
	p := Perf{NumCPU: runtime.NumCPU(), MemAvailMB: memAvailableMB()}
	p.HashMBPerSec = benchmark(150 * time.Millisecond)

	p.Workers = clamp(p.NumCPU, 2, 8)
	p.LibConc, p.Prewarm = 2, 2
	switch {
	case p.MemAvailMB < 0: // 未知,不施加内存约束
	case p.MemAvailMB < 500:
		p.Workers = clamp(p.Workers, 2, 2)
		p.LibConc, p.Prewarm = 1, 1
	case p.MemAvailMB < 1024:
		p.Workers = clamp(p.Workers, 2, 4)
		p.LibConc, p.Prewarm = 1, 1
	case p.MemAvailMB < 2048:
		p.Workers = clamp(p.Workers, 2, 4)
	}
	// 慢 CPU:worker 减半,保底 2,避免扫描饿死前台请求
	if p.HashMBPerSec > 0 && p.HashMBPerSec < slowThresholdMBps && p.Workers > 2 {
		p.Workers = clamp(p.Workers/2, 2, p.Workers)
	}
	p.DBConns = clamp(p.Workers+1, 2, 8)
	return p
}

// Log 输出决策依据。
func (p Perf) Log() {
	mem := "未知"
	if p.MemAvailMB >= 0 {
		mem = strconv.Itoa(p.MemAvailMB) + "MB"
	}
	if p.Overridden {
		log.Printf("[perf] CPU=%d核 基准=%.0fMB/s 内存可用=%s → 扫描并发=%d(手动覆盖) 库并发=%d 预热=%d",
			p.NumCPU, p.HashMBPerSec, mem, p.Workers, p.LibConc, p.Prewarm)
		return
	}
	log.Printf("[perf] CPU=%d核 基准=%.0fMB/s 内存可用=%s → 扫描并发=%d 库并发=%d 预热=%d",
		p.NumCPU, p.HashMBPerSec, mem, p.Workers, p.LibConc, p.Prewarm)
}

// benchmark 测单核 SHA-256 吞吐(MB/s),代表扫描的哈希/压缩负载特征。
func benchmark(d time.Duration) float64 {
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i * 31)
	}
	h := sha256.New()
	deadline := time.Now().Add(d)
	rounds := 0
	for time.Now().Before(deadline) {
		h.Reset()
		h.Write(buf)
		h.Sum(nil)
		rounds++
	}
	if rounds == 0 {
		return 0
	}
	return float64(rounds) / d.Seconds()
}

// memAvailableMB 读取 /proc/meminfo 的 MemAvailable;非 Linux 或失败返回 -1。
func memAvailableMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return int(kb / 1024)
				}
			}
		}
	}
	return -1
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
