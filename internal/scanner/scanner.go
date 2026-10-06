// Package scanner 扫描库目录:一级子目录=系列,目录内 zip/cbz=册。
// 扫描只建索引(读 zip 目录、页数、尺寸、哈希),封面懒生成 + 后台预热。
// 库间并发(上限 2)+ 库内 worker 池(CPU 数)。
package scanner

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"

	"komga-lite/internal/perf"
	"komga-lite/internal/store"
	"komga-lite/internal/ziputil"
)

var ErrBusy = errors.New("library is being scanned")

var numRe = regexp.MustCompile(`\d+(\.\d+)?`)

const batchSize = 50

type Scanner struct {
	st   *store.Store
	mu   sync.Mutex
	busy map[int64]bool

	workers     int           // 单库扫描 worker 数(按机器性能定)
	libSem      chan struct{} // 库级并发信号量
	warmSem     chan struct{} // 预热并发信号量
	warmWorkers int           // 预热 worker 数
}

// New 按探测出的性能档位创建扫描器。
func New(st *store.Store, pf perf.Perf) *Scanner {
	return &Scanner{
		st:          st,
		busy:        map[int64]bool{},
		workers:     pf.Workers,
		libSem:      make(chan struct{}, pf.LibConc),
		warmSem:     make(chan struct{}, 1),
		warmWorkers: pf.Prewarm,
	}
}

// ScanAll 并发扫描所有库。
func (s *Scanner) ScanAll() {
	libs, err := s.st.ListLibraries()
	if err != nil {
		log.Printf("[scan] 读取库列表失败: %v", err)
		return
	}
	var wg sync.WaitGroup
	for _, l := range libs {
		wg.Add(1)
		go func(id int64, name string) {
			defer wg.Done()
			s.libSem <- struct{}{}
			defer func() { <-s.libSem }()
			if err := s.ScanLibrary(id); err != nil {
				log.Printf("[scan] 库 %q: %v", name, err)
			}
		}(l.ID, l.Name)
	}
	wg.Wait()
}

// ScanAsync 异步触发一次扫描,重复触发时静默忽略。
func (s *Scanner) ScanAsync(id int64) {
	go func() {
		if err := s.ScanLibrary(id); err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, os.ErrNotExist) {
			log.Printf("[scan] library %d: %v", id, err)
		}
	}()
}

type bookJob struct {
	seriesID int64
	path     string
}

type bookResult struct {
	up      store.BookUp
	changed bool
	err     error
}

// ScanLibrary 同步扫描一个库;正在扫描时返回 ErrBusy。
func (s *Scanner) ScanLibrary(id int64) error {
	s.mu.Lock()
	if s.busy[id] {
		s.mu.Unlock()
		return ErrBusy
	}
	s.busy[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.busy, id)
		s.mu.Unlock()
	}()

	start := time.Now()
	lib, err := s.st.GetLibrary(id)
	if err != nil {
		return err
	}
	if lib == nil {
		return os.ErrNotExist
	}
	entries, err := os.ReadDir(lib.Root)
	if err != nil {
		return err
	}

	// 1) 收集任务:系列入库 + 书籍文件列表
	var jobs []bookJob
	type seriesInfo struct {
		id   int64
		name string
		kept []string
	}
	var seriesList []seriesInfo
	var rootKept []string // 库根目录下散装的 zip/cbz,合并入以根目录名命名的系列
	var rootFlm int64
	if info, ierr := os.Stat(lib.Root); ierr == nil {
		rootFlm = info.ModTime().UTC().Unix()
	} else {
		rootFlm = time.Now().UTC().Unix()
	}
	for _, e := range entries {
		if !e.IsDir() {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == ".zip" || ext == ".cbz" {
				rootKept = append(rootKept, filepath.Join(lib.Root, e.Name()))
			}
			continue
		}
		name := e.Name()
		var flm int64
		if info, ierr := e.Info(); ierr == nil {
			flm = info.ModTime().UTC().Unix()
		} else {
			flm = time.Now().UTC().Unix()
		}
		seriesID, err := s.st.UpsertSeries(lib.ID, name, flm)
		if err != nil {
			log.Printf("[scan] 系列 %q 入库失败: %v", name, err)
			continue
		}
		files, err := os.ReadDir(filepath.Join(lib.Root, name))
		if err != nil {
			log.Printf("[scan] 读取目录失败 %s: %v", name, err)
			continue
		}
		si := seriesInfo{id: seriesID, name: name}
		for _, fe := range files {
			if fe.IsDir() {
				continue
			}
			fp := filepath.Join(lib.Root, name, fe.Name())
			si.kept = append(si.kept, fp)
			jobs = append(jobs, bookJob{seriesID: seriesID, path: fp})
		}
		seriesList = append(seriesList, si)
	}

	// 根目录散装书合并入以根目录名命名的系列(与同名子目录系列合并)
	if len(rootKept) > 0 {
		name := filepath.Base(lib.Root)
		seriesID, err := s.st.UpsertSeries(lib.ID, name, rootFlm)
		if err != nil {
			log.Printf("[scan] 根目录系列 %q 入库失败: %v", name, err)
		} else {
			for _, fp := range rootKept {
				jobs = append(jobs, bookJob{seriesID: seriesID, path: fp})
			}
			seriesList = append(seriesList, seriesInfo{id: seriesID, name: name, kept: rootKept})
		}
	}

	// 2) worker 池分析(纯读文件+CPU,不写库),单 writer 批量落盘
	workers := s.workers
	jobsCh := make(chan bookJob)
	resCh := make(chan bookResult, workers)
	var wp sync.WaitGroup
	for i := 0; i < workers; i++ {
		wp.Add(1)
		go func() {
			defer wp.Done()
			for j := range jobsCh {
				up, changed, err := s.analyzeBook(j.seriesID, j.path)
				resCh <- bookResult{up: up, changed: changed, err: err}
			}
		}()
	}
	var added, errs int
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		var batch []store.BookWrite
		flush := func() {
			if err := s.st.FlushBooks(batch); err != nil {
				log.Printf("[scan] 批量写库失败: %v", err)
			}
			batch = batch[:0]
		}
		for res := range resCh {
			if res.err != nil {
				log.Printf("[scan] %s: %v", res.up.FilePath, res.err)
				errs++
				continue
			}
			if res.changed {
				added++
				batch = append(batch, store.BookWrite{Up: res.up, InvalidateThumb: true})
				if len(batch) >= batchSize {
					flush()
				}
			}
		}
		flush()
	}()
	for _, j := range jobs {
		jobsCh <- j
	}
	close(jobsCh)
	wp.Wait()
	close(resCh)
	<-writerDone

	// 3) 清理失效数据(书库若在扫描中被删除,则放弃本次结果)
	if lib2, err := s.st.GetLibrary(id); err == nil && lib2 == nil {
		log.Printf("[scan] 库 %q 在扫描期间被删除,本次结果作废", lib.Name)
		return nil
	}
	for _, si := range seriesList {
		if err := s.st.DeleteMissingBooks(si.id, si.kept); err != nil {
			log.Printf("[scan] 清理失效书籍失败: %v", err)
		}
	}
	keepNames := make([]string, 0, len(seriesList))
	for _, si := range seriesList {
		keepNames = append(keepNames, si.name)
	}
	if err := s.st.DeleteMissingSeries(lib.ID, keepNames); err != nil {
		log.Printf("[scan] 清理失效系列失败: %v", err)
	}
	log.Printf("[scan] 库 %q 索引完成:新增/更新 %d 册,错误 %d,耗时 %v", lib.Name, added, errs, time.Since(start).Round(time.Millisecond))

	// 4) 后台预热封面(低优先级,不阻塞)
	go s.prewarm(lib.Name)
	return nil
}

// analyzeBook 只做索引:zip 目录、页数/尺寸、哈希;不生成封面。
func (s *Scanner) analyzeBook(seriesID int64, path string) (store.BookUp, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return store.BookUp{FilePath: path}, false, err
	}
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	ext := strings.ToLower(filepath.Ext(path))

	// 未变化则跳过
	if id, size, mtime, err := s.st.BookStat(path); err != nil {
		return store.BookUp{FilePath: path}, false, err
	} else if id != 0 && size == info.Size() && mtime == info.ModTime().Unix() {
		return store.BookUp{FilePath: path}, false, nil
	}

	up := store.BookUp{
		SeriesID:         seriesID,
		Name:             name,
		Number:           parseNumber(name),
		FilePath:         path,
		FileSize:         info.Size(),
		FileMTime:        info.ModTime().Unix(),
		FileLastModified: info.ModTime().UTC().Unix(),
		MediaStatus:      "UNSUPPORTED",
		FileHash:         fileSHA256(path),
	}
	if ext == ".zip" || ext == ".cbz" {
		up.MediaType = sql.NullString{String: "CBZ", Valid: true}
		pages, err := ziputil.ListPages(path)
		switch {
		case err != nil:
			up.MediaStatus = "ERROR"
		case len(pages) == 0:
			up.MediaStatus = "ERROR" // 压缩包内没有图片
		default:
			up.MediaStatus = "READY"
			up.PagesCount = sql.NullInt64{Int64: int64(len(pages)), Valid: true}
			if j, err := json.Marshal(pages); err == nil {
				up.PagesJSON = sql.NullString{String: string(j), Valid: true}
			}
		}
	}
	return up, true, nil
}

// prewarm 为没有封面的书补生成封面(每批 100 本,与扫描共用忙等,不挡前台)。
func (s *Scanner) prewarm(libName string) {
	s.warmSem <- struct{}{}
	defer func() { <-s.warmSem }()
	done := 0
	for {
		refs, err := s.st.BooksMissingThumb(100)
		if err != nil {
			log.Printf("[warm] 查询缺封面书籍失败: %v", err)
			return
		}
		if len(refs) == 0 {
			break
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, s.warmWorkers)
		for _, r := range refs {
			wg.Add(1)
			sem <- struct{}{}
			go func(r store.BookRef) {
				defer wg.Done()
				defer func() { <-sem }()
				if data, err := GenerateThumbnail(r.FilePath); err == nil {
					if err := s.st.ThumbPutGenerated(r.ID, data); err == nil {
						done++
					}
				}
			}(r)
		}
		wg.Wait()
		if len(refs) < 100 {
			break
		}
	}
	if done > 0 {
		log.Printf("[warm] 库 %q 封面预热完成:新生成 %d 张", libName, done)
	}
}

// GenerateThumbnail 从 zip 第一页生成 300px 宽 JPEG 封面(API 懒生成也用它)。
func GenerateThumbnail(path string) ([]byte, error) {
	data, _, err := ziputil.ReadPage(path, 0)
	if err != nil {
		return nil, err
	}
	return scaleThumb(data, 300)
}

func scaleThumb(data []byte, width int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w := width
	if b.Dx() <= w {
		w = b.Dx()
	}
	h := b.Dy() * w / b.Dx()
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// parseNumber 从文件名中提取卷号/话数,如 "003 卷" -> 3;没有数字则为 NULL。
func parseNumber(name string) sql.NullFloat64 {
	m := numRe.FindString(name)
	if m == "" {
		return sql.NullFloat64{}
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: f, Valid: true}
}

// fileSHA256 计算文件内容哈希(Komga 用作同步标识);失败返回空串。
func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
