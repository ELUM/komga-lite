// mktestdata 生成一个用于冒烟测试的迷你书库:
// 两个系列共 6 本 zip 漫画,其中一本使用 GBK 编码的条目名。
package main

import (
	"archive/zip"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func main() {
	root := filepath.Join("testdata", "library")
	if err := os.RemoveAll(root); err != nil {
		log.Fatal(err)
	}
	s1 := filepath.Join(root, "测试系列 Alpha")
	s2 := filepath.Join(root, "Second Series β")
	books := []struct {
		dir       string
		name      string
		pages     int
		gbkNames  bool
		nestedDir bool
	}{
		{s1, "001", 5, false, false},
		{s1, "002", 4, false, false},
		{s1, "003", 3, true, true}, // GBK 文件名 + 子目录
		{s2, "vol 1", 6, false, false},
		{s2, "vol 2", 3, false, false},
		{s2, "vol 10", 2, false, false}, // 验证自然排序: vol 2 < vol 10
	}
	for _, b := range books {
		if err := os.MkdirAll(b.dir, 0o755); err != nil {
			log.Fatal(err)
		}
		path := filepath.Join(b.dir, b.name+".zip")
		if err := writeBook(path, b.pages, b.gbkNames, b.nestedDir); err != nil {
			log.Fatalf("%s: %v", path, err)
		}
		fmt.Println("生成", path)
	}
	// 一个坏 zip:应被标记为 ERROR 而不是拖垮扫描
	bad := filepath.Join(s1, "broken.zip")
	os.WriteFile(bad, []byte("this is not a zip file"), 0o644)
	fmt.Println("生成", bad)
	// 一个不支持的格式:应标记为 UNSUPPORTED
	unsup := filepath.Join(s1, "notes.txt")
	os.WriteFile(unsup, []byte("hello"), 0o644)
	fmt.Println("生成", unsup)
}

func writeBook(path string, pages int, gbk, nested bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for p := 1; p <= pages; p++ {
		name := fmt.Sprintf("page_%02d.png", p)
		if nested {
			name = "sub/" + name
		}
		if gbk {
			enc, err := simplifiedchinese.GBK.NewEncoder().String(fmt.Sprintf("第%02d页.png", p))
			if err == nil {
				name = enc
			}
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			return err
		}
		if err := png.Encode(w, pageImage(p)); err != nil {
			return err
		}
	}
	return zw.Close()
}

// pageImage 生成纯色底+黑白格条纹的测试页,条纹随页码变化,肉眼可分辨页序。
func pageImage(n int) image.Image {
	const w, h = 240, 340
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	base := color.RGBA{R: uint8(30 + n*35), G: uint8(90 + n*20), B: 180, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if y >= h-40 && (x/24+n)%2 == 0 {
				img.Set(x, y, color.Black)
			} else {
				img.Set(x, y, base)
			}
		}
	}
	return img
}
