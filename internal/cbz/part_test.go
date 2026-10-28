package cbz_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/tonyupup/pdfcbzfs/internal/cbz"
)

// memSource serves images from memory; the "file" argument is ignored.
type memSource struct{ imgs [][]byte }

func (m memSource) ReadImageAt(_ io.ReaderAt, page int, p []byte, off int64) (int, error) {
	img := m.imgs[page]
	if off >= int64(len(img)) {
		return 0, io.EOF
	}
	n := copy(p, img[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func images(n int) memSource {
	rng := rand.New(rand.NewSource(1))
	var s memSource
	for i := 0; i < n; i++ {
		b := make([]byte, 1000+rng.Intn(5000))
		rng.Read(b)
		s.imgs = append(s.imgs, b)
	}
	return s
}

func build(t *testing.T, src memSource, pages []int, info *cbz.Info) *cbz.Part {
	t.Helper()
	var imgs []cbz.Img
	for _, p := range pages {
		imgs = append(imgs, cbz.Img{Page: p, Size: int64(len(src.imgs[p]))})
	}
	p, err := cbz.NewPart(src, imgs, info, time.Date(2024, 3, 4, 5, 6, 8, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type readerAt struct {
	p *cbz.Part
}

func (r readerAt) ReadAt(b []byte, off int64) (int, error) { return r.p.ReadAt(nil, b, off) }

func full(t *testing.T, p *cbz.Part) []byte {
	t.Helper()
	b := make([]byte, p.Size())
	n, err := p.ReadAt(nil, b, 0)
	if int64(n) != p.Size() || (err != nil && err != io.EOF) {
		t.Fatalf("full read: n=%d size=%d err=%v", n, p.Size(), err)
	}
	return b
}

func TestZipIsValidAndBytesMatch(t *testing.T) {
	src := images(10)
	pages := []int{3, 4, 5, 6, 7}
	p := build(t, src, pages, &cbz.Info{Series: "A & B", Number: 2})
	data := full(t, p)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != len(pages)+1 {
		t.Fatalf("entries = %d, want %d", len(zr.File), len(pages)+1)
	}
	if zr.File[0].Name != "ComicInfo.xml" {
		t.Fatalf("first entry %q", zr.File[0].Name)
	}
	for i, pg := range pages {
		f := zr.File[i+1]
		if want := []string{"0001.jpg", "0002.jpg", "0003.jpg", "0004.jpg", "0005.jpg"}[i]; f.Name != want {
			t.Fatalf("entry %d named %q, want %q", i, f.Name, want)
		}
		if f.Method != zip.Store {
			t.Fatalf("%s not stored", f.Name)
		}
		rc, _ := f.Open()
		got, err := io.ReadAll(rc) // the zip reader verifies the CRC32 here
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if !bytes.Equal(got, src.imgs[pg]) {
			t.Fatalf("%s: content differs", f.Name)
		}
	}
	rc, _ := zr.File[0].Open()
	info, _ := io.ReadAll(rc)
	if !bytes.Contains(info, []byte("<Series>A &amp; B</Series>")) || !bytes.Contains(info, []byte("<Number>2</Number>")) {
		t.Fatalf("bad ComicInfo: %s", info)
	}
}

func TestNoComicInfo(t *testing.T) {
	src := images(3)
	p := build(t, src, []int{0, 1, 2}, nil)
	data := full(t, p)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(zr.File) != 3 || zr.File[0].Name != "0001.jpg" {
		t.Fatalf("err=%v files=%d", err, len(zr.File))
	}
}

func TestRandomAccess(t *testing.T) {
	src := images(8)
	p := build(t, src, []int{0, 1, 2, 3, 4, 5, 6, 7}, &cbz.Info{Series: "S", Number: 1})
	want := full(t, p)
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 500; i++ {
		off := rng.Int63n(p.Size())
		n := 1 + rng.Intn(9000)
		buf := make([]byte, n)
		m, err := p.ReadAt(nil, buf, off)
		end := off + int64(n)
		if end > p.Size() {
			end = p.Size()
		}
		if int64(m) != end-off {
			t.Fatalf("off=%d n=%d: got %d bytes (err %v), want %d", off, n, m, err, end-off)
		}
		if !bytes.Equal(buf[:m], want[off:end]) {
			t.Fatalf("off=%d n=%d: bytes differ", off, n)
		}
		if int64(m) < int64(n) && err != io.EOF {
			t.Fatalf("short read without EOF: %v", err)
		}
	}
	// reading the central directory first (as unzip does) must work cold
	p2 := build(t, src, []int{0, 1, 2}, nil)
	tail := make([]byte, 22)
	if _, err := p2.ReadAt(nil, tail, p2.Size()-22); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if string(tail[:4]) != "PK\x05\x06" {
		t.Fatalf("EOCD signature missing: % x", tail[:4])
	}
}

func TestReadPastEnd(t *testing.T) {
	p := build(t, images(2), []int{0, 1}, nil)
	n, err := p.ReadAt(nil, make([]byte, 4), p.Size())
	if n != 0 || err != io.EOF {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestZipReaderOverReaderAt(t *testing.T) {
	p := build(t, images(5), []int{0, 1, 2, 3, 4}, nil)
	zr, err := zip.NewReader(readerAt{p}, p.Size())
	if err != nil || len(zr.File) != 5 {
		t.Fatalf("err=%v", err)
	}
}

func TestTooBig(t *testing.T) {
	_, err := cbz.NewPart(memSource{}, []cbz.Img{{Page: 0, Size: 1 << 33}}, nil, time.Now())
	if !errors.Is(err, cbz.ErrTooBig) {
		t.Fatalf("err = %v", err)
	}
}

func TestExtensions(t *testing.T) {
	src := images(2)
	p, err := cbz.NewPart(src, []cbz.Img{
		{Page: 0, Size: int64(len(src.imgs[0])), Ext: "png"},
		{Page: 1, Size: int64(len(src.imgs[1])), Ext: "jpg"},
	}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data := full(t, p)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || zr.File[0].Name != "0001.png" || zr.File[1].Name != "0002.jpg" {
		t.Fatalf("err=%v names=%v", err, zr.File)
	}
}

// slowSource reports that its reads are CPU-bound, so the CRC pass may run
// several at once.
type slowSource struct {
	memSource
	par    int
	mu     sync.Mutex
	active int
	peak   int
	fail   map[int]error
}

func (s *slowSource) ReadParallelism() int { return s.par }

func (s *slowSource) ReadImageAt(r io.ReaderAt, page int, p []byte, off int64) (int, error) {
	s.mu.Lock()
	s.active++
	s.peak = max(s.peak, s.active)
	err := s.fail[page]
	s.mu.Unlock()
	time.Sleep(15 * time.Millisecond)
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return s.memSource.ReadImageAt(r, page, p, off)
}

func TestCRCPassRunsInParallelWhenTheSourceAsksForIt(t *testing.T) {
	src := &slowSource{memSource: images(12), par: 3}
	var imgs []cbz.Img
	for i := range src.imgs {
		imgs = append(imgs, cbz.Img{Page: i, Size: int64(len(src.imgs[i]))})
	}
	p, err := cbz.NewPart(src, imgs, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// zip readers start with the central directory at the end of the file,
	// which needs every CRC at once
	tail := make([]byte, 100)
	if _, err := p.ReadAt(nil, tail, p.Size()-100); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, p.Size())
	if n, err := p.ReadAt(nil, b, 0); n != len(b) {
		t.Fatalf("read %d of %d: %v", n, len(b), err)
	}
	if src.peak < 2 || src.peak > 3 {
		t.Fatalf("peak concurrency %d, want 2 or 3", src.peak)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range zr.File {
		rc, _ := f.Open()
		got, err := io.ReadAll(rc) // verifies the CRC the parallel pass computed
		if err != nil || !bytes.Equal(got, src.imgs[i]) {
			t.Fatalf("%s: err %v", f.Name, err)
		}
	}
}

func TestSequentialSourcesStaySequential(t *testing.T) {
	src := &slowSource{memSource: images(8), par: 1}
	var imgs []cbz.Img
	for i := range src.imgs {
		imgs = append(imgs, cbz.Img{Page: i, Size: int64(len(src.imgs[i]))})
	}
	p, _ := cbz.NewPart(src, imgs, nil, time.Now())
	b := make([]byte, 100)
	p.ReadAt(nil, b, p.Size()-100)
	if src.peak != 1 {
		t.Fatalf("a source that does not ask for parallel reads got %d at once", src.peak)
	}
}

func TestParallelCRCPassReportsFailureAndRecovers(t *testing.T) {
	boom := errors.New("renderer died")
	src := &slowSource{memSource: images(6), par: 3, fail: map[int]error{4: boom}}
	var imgs []cbz.Img
	for i := range src.imgs {
		imgs = append(imgs, cbz.Img{Page: i, Size: int64(len(src.imgs[i]))})
	}
	p, _ := cbz.NewPart(src, imgs, nil, time.Now())
	b := make([]byte, 100)
	if _, err := p.ReadAt(nil, b, p.Size()-100); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	src.mu.Lock()
	src.fail = nil // the failure was transient
	src.mu.Unlock()
	if n, err := p.ReadAt(nil, b, p.Size()-100); n != 100 || err != nil {
		t.Fatalf("after the failure went away: n %d err %v (a failed CRC must not be cached)", n, err)
	}
}
