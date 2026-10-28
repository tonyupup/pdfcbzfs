package render

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gradient builds a smooth test page through the same builder readPPM uses.
func gradient(w, h int, gray bool) *Raster {
	b := newRasterBuilder(w, h)
	row := make([]byte, 3*w)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if gray {
				v := byte(x*255/w + y%3)
				row[3*x], row[3*x+1], row[3*x+2] = v, v, v
			} else {
				row[3*x], row[3*x+1], row[3*x+2] = byte(x*255/w), byte(y*255/h), byte((x+y)%64*4)
			}
		}
		b.addRow(y, row)
	}
	return b.finish()
}

func TestFitJPEGIsExactAndValid(t *testing.T) {
	r := gradient(120, 90, false)
	base, err := FitJPEG(r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// every pad size around the interesting boundaries, including 1 to 3
	// leftover bytes and several COM segments
	for _, extra := range []int{0, 1, 2, 3, 4, 5, 6, 7, 100, 65536, 65537, 65538, 65539, 65540, 65541, 65542, 131074, 200000} {
		// find the unpadded length first by fitting with a budget just big enough
		raw := unpadded(t, r)
		budget := int64(len(raw) + extra)
		got, err := FitJPEG(r, budget)
		if err != nil {
			t.Fatalf("extra %d: %v", extra, err)
		}
		if int64(len(got)) != budget {
			t.Fatalf("extra %d: len %d, want %d", extra, len(got), budget)
		}
		img, err := jpeg.Decode(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("extra %d: padded JPEG does not decode: %v", extra, err)
		}
		if b := img.Bounds(); b.Dx() != 120 || b.Dy() != 90 {
			t.Fatalf("extra %d: size %v", extra, b)
		}
	}
	_ = base
}

// unpadded returns the length the encoder produces at the first quality FitJPEG tries.
func unpadded(t *testing.T, r *Raster) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, r.image(), &jpeg.Options{Quality: 82}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFitJPEGLowersQualityThenShrinks(t *testing.T) {
	r := gradient(300, 300, false)
	full := unpadded(t, r) // 10481 bytes at quality 82; 6162 at the lowest quality tried
	if len(full) < 9000 {
		t.Fatalf("test image changed: %d bytes", len(full))
	}
	// between two quality steps: a lower quality is enough, the size stays
	got, err := FitJPEG(r, 8000)
	if err != nil || len(got) != 8000 {
		t.Fatalf("len %d err %v", len(got), err)
	}
	img, _ := jpeg.Decode(bytes.NewReader(got))
	if img.Bounds().Dx() != 300 {
		t.Fatalf("should keep full size when quality is enough, got %v", img.Bounds())
	}
	// below what the lowest quality can do: the page is shrunk
	small, err := FitJPEG(r, 4000)
	if err != nil || len(small) != 4000 {
		t.Fatalf("len %d err %v", len(small), err)
	}
	img, err = jpeg.Decode(bytes.NewReader(small))
	if err != nil || img.Bounds().Dx() >= 300 {
		t.Fatalf("expected a shrunken image, got %v (err %v)", img.Bounds(), err)
	}
	if _, err := FitJPEG(r, 300); !errors.Is(err, ErrTooBig) {
		t.Fatalf("impossible budget: err = %v", err)
	}
}

func TestFitJPEGGray(t *testing.T) {
	got, err := FitJPEG(gradient(64, 64, true), 20000)
	if err != nil || len(got) != 20000 {
		t.Fatalf("len %d err %v", len(got), err)
	}
	img, err := jpeg.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := img.(*image.Gray); !ok {
		t.Fatalf("gray raster should encode as a gray JPEG, got %T", img)
	}
	_ = color.Gray{}
}

func TestCacheRoundTripAndWrongSize(t *testing.T) {
	c := NewCache(t.TempDir(), 1<<20)
	if _, ok := c.Get("k", 3); ok {
		t.Fatal("empty cache hit")
	}
	c.Put("k", []byte("abc"))
	if b, ok := c.Get("k", 3); !ok || string(b) != "abc" {
		t.Fatalf("got %q %v", b, ok)
	}
	if _, ok := c.Get("k", 4); ok {
		t.Fatal("an entry of the wrong size must be discarded")
	}
	if _, ok := c.Get("k", 3); ok {
		t.Fatal("the wrong-size entry should have been removed")
	}
	var nilCache *Cache
	nilCache.Put("x", []byte("y"))
	if _, ok := nilCache.Get("x", 1); ok {
		t.Fatal("nil cache must miss")
	}
}

func TestCacheEvictsOldestOverLimit(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{dir: dir, max: 1000}
	for i := 0; i < 10; i++ {
		p := c.path(string(rune('a' + i)))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, make([]byte, 200), 0o644)
		os.Chtimes(p, time.Unix(1000+int64(i), 0), time.Unix(1000+int64(i), 0))
	}
	c.gc()
	var left int
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			left++
		}
		return nil
	})
	if left != 4 { // 2000 bytes down to <= 800
		t.Fatalf("%d entries left, want 4", left)
	}
	if _, ok := c.Get("j", 200); !ok {
		t.Fatal("the newest entry must survive")
	}
	if _, ok := c.Get("a", 200); ok {
		t.Fatal("the oldest entry must be gone")
	}
}

type fakeBackend struct {
	mu       sync.Mutex
	calls    int
	failures []error // consumed one per call, then success
	checkErr error
	delay    time.Duration
	active   atomic.Int32
	peak     atomic.Int32
}

func (f *fakeBackend) Check() error { return f.checkErr }
func (f *fakeBackend) Render(ctx context.Context, q Request) (*Raster, error) {
	n := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(f.delay)
	f.mu.Lock()
	f.calls++
	var err error
	if len(f.failures) > 0 {
		err, f.failures = f.failures[0], f.failures[1:]
	}
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return gradient(40, 30, false), nil
}

func svc(be Backend, dir string) (*Service, *[]time.Duration) {
	var slept []time.Duration
	return New(Config{Backend: be, CacheDir: dir, MaxCache: 1 << 20, Workers: 2,
		Sleep: func(d time.Duration) { slept = append(slept, d) }}), &slept
}

func key(page int) PageKey {
	return PageKey{FileID: "f1", Path: "/x.pdf", Page: page, Budget: 30000, DPI: 150, MaxSide: 3000}
}

func TestServiceRetriesTransientFailuresWithBackoff(t *testing.T) {
	fb := &fakeBackend{failures: []error{&Error{Msg: "oom killed"}, &Error{Msg: "oom killed"}}}
	s, slept := svc(fb, "")
	b, err := s.Page(context.Background(), key(0))
	if err != nil || len(b) != 30000 {
		t.Fatalf("len %d err %v", len(b), err)
	}
	if fb.calls != 3 {
		t.Fatalf("%d calls, want 3 (two failures, one success)", fb.calls)
	}
	if len(*slept) != 2 || (*slept)[0] != 400*time.Millisecond || (*slept)[1] != 1500*time.Millisecond {
		t.Fatalf("backoff = %v", *slept)
	}
}

func TestServiceDoesNotRetryPermanentFailures(t *testing.T) {
	fb := &fakeBackend{failures: []error{&Error{Msg: "wrong password", Permanent: true}}}
	s, slept := svc(fb, "")
	if _, err := s.Page(context.Background(), key(0)); err == nil {
		t.Fatal("want an error")
	}
	if fb.calls != 1 || len(*slept) != 0 {
		t.Fatalf("calls %d slept %v", fb.calls, *slept)
	}
}

func TestServiceGivesUpAfterThreeAttempts(t *testing.T) {
	e := &Error{Msg: "boom"}
	fb := &fakeBackend{failures: []error{e, e, e, e, e}}
	s, _ := svc(fb, "")
	if _, err := s.Page(context.Background(), key(0)); err == nil || fb.calls != 3 {
		t.Fatalf("err %v calls %d", err, fb.calls)
	}
}

func TestServiceUsesDiskCacheAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	fb := &fakeBackend{}
	s, _ := svc(fb, dir)
	a, err := s.Page(context.Background(), key(2))
	if err != nil {
		t.Fatal(err)
	}
	fb2 := &fakeBackend{failures: []error{&Error{Msg: "must not be called"}}}
	s2, _ := svc(fb2, dir) // a restart
	b, err := s2.Page(context.Background(), key(2))
	if err != nil || !bytes.Equal(a, b) || fb2.calls != 0 {
		t.Fatalf("cache miss after restart: calls %d err %v", fb2.calls, err)
	}
	other := key(2)
	other.FileID = "f2" // the file changed
	if _, err := s2.Page(context.Background(), other); err != nil || fb2.calls == 0 {
		t.Fatalf("a changed file must not hit the old entry (calls %d, err %v)", fb2.calls, err)
	}
}

func TestServiceSharesWorkAndCapsConcurrency(t *testing.T) {
	fb := &fakeBackend{delay: 60 * time.Millisecond}
	s, _ := svc(fb, "")
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ { // the same page five times at once
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Page(context.Background(), key(7)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if fb.calls != 1 {
		t.Fatalf("same page rendered %d times", fb.calls)
	}
	fb2 := &fakeBackend{delay: 60 * time.Millisecond}
	s2, _ := svc(fb2, "")
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s2.Page(context.Background(), key(i))
		}(i)
	}
	wg.Wait()
	if p := fb2.peak.Load(); p > 2 || p < 1 {
		t.Fatalf("peak concurrency %d with 2 workers", p)
	}
}

func TestServiceAvailabilityBacksOff(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	fb := &fakeBackend{checkErr: errors.New("libpdfium.so missing")}
	s := New(Config{Backend: fb, Now: func() time.Time { return now }})
	if s.Available() {
		t.Fatal("broken helper must not be available")
	}
	checks := 0
	fb2 := &countingCheck{err: errors.New("still broken")}
	s = New(Config{Backend: fb2, Now: func() time.Time { return now }})
	for i := 0; i < 5; i++ { // a burst of probes inside the first backoff window
		s.Available()
	}
	if fb2.n != 1 {
		t.Fatalf("%d checks in one window, want 1", fb2.n)
	}
	for _, step := range []time.Duration{16 * time.Second, 31 * time.Second, 61 * time.Second, 121 * time.Second} {
		now = now.Add(step)
		before := fb2.n
		s.Available()
		if fb2.n != before+1 {
			t.Fatalf("no re-check after %v", step)
		}
		checks++
	}
	fb2.err = nil // the helper gets installed
	now = now.Add(11 * time.Minute)
	if !s.Available() {
		t.Fatal("must become available once the check passes")
	}
	if s.Why() != nil {
		t.Fatalf("why = %v", s.Why())
	}
}

type countingCheck struct {
	fakeBackend
	n   int
	err error
}

func (c *countingCheck) Check() error { c.n++; return c.err }

func TestHelperEndToEnd(t *testing.T) {
	h := FindHelper()
	if h == nil || h.Check() != nil {
		t.Skip("pdfcbzfs-render is not installed")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip()
	}
	_, err := h.Render(context.Background(), Request{Path: "/etc/hostname", Page: 0, DPI: 72, MaxSide: 500})
	var re *Error
	if !errors.As(err, &re) || !re.Permanent {
		t.Fatalf("a non-PDF must be a permanent error, got %v", err)
	}
}

func ppm(w, h int, px func(x, y int) (r, g, b byte)) []byte {
	out := []byte(fmt.Sprintf("P6\n%d %d\n255\n", w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b := px(x, y)
			out = append(out, r, g, b)
		}
	}
	return out
}

func TestReadPPMPlanesAndColour(t *testing.T) {
	for _, dim := range [][2]int{{64, 48}, {63, 47}, {1, 1}, {2, 1}, {5, 7}} { // odd sizes too
		w, h := dim[0], dim[1]
		// a flat orange page: after the YCbCr round trip it must stay orange
		r, err := readPPM(bufio.NewReader(bytes.NewReader(ppm(w, h, func(x, y int) (byte, byte, byte) { return 230, 120, 20 }))))
		if err != nil {
			t.Fatalf("%dx%d: %v", w, h, err)
		}
		if r.Gray || r.W != w || r.H != h || len(r.Y) != w*h || r.CW != (w+1)/2 || len(r.Cb) != r.CW*r.CH {
			t.Fatalf("%dx%d: unexpected raster %+v", w, h, r)
		}
		jpg, err := FitJPEG(r, 20000)
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(bytes.NewReader(jpg))
		if err != nil {
			t.Fatalf("%dx%d: %v", w, h, err)
		}
		cr, cg, cb, _ := img.At(w/2, h/2).RGBA()
		for i, p := range [][2]uint32{{cr >> 8, 230}, {cg >> 8, 120}, {cb >> 8, 20}} {
			if d := int(p[0]) - int(p[1]); d < -6 || d > 6 {
				t.Fatalf("%dx%d: channel %d = %d, want about %d", w, h, i, p[0], p[1])
			}
		}
	}
}

func TestReadPPMDetectsGray(t *testing.T) {
	gray, err := readPPM(bufio.NewReader(bytes.NewReader(ppm(33, 21, func(x, y int) (byte, byte, byte) { v := byte(x * 7); return v, v, v }))))
	if err != nil || !gray.Gray || gray.Cb != nil || len(gray.Y) != 33*21 {
		t.Fatalf("gray page: %+v err %v", gray, err)
	}
	if gray.Y[5] != 35 { // Y of a gray pixel is the pixel itself
		t.Fatalf("Y = %d, want 35", gray.Y[5])
	}
	col, _ := readPPM(bufio.NewReader(bytes.NewReader(ppm(33, 21, func(x, y int) (byte, byte, byte) {
		if x == 30 && y == 20 { // one coloured pixel in the last corner
			return 200, 10, 10
		}
		return 255, 255, 255
	}))))
	if col.Gray {
		t.Fatal("a single coloured pixel makes the page colour")
	}
	if _, err := readPPM(bufio.NewReader(bytes.NewReader([]byte("P6\n10 10\n255\nshort")))); err == nil {
		t.Fatal("truncated image must fail")
	}
	if _, err := readPPM(bufio.NewReader(bytes.NewReader([]byte("P5\n10 10\n255\n")))); err == nil {
		t.Fatal("wrong magic must fail")
	}
}

func TestShrinkKeepsPlanesConsistent(t *testing.T) {
	r := gradient(101, 77, false)
	small := shrink(r.image(), 0.5).(*image.YCbCr)
	b := small.Bounds()
	if b.Dx() != 50 || b.Dy() != 38 || len(small.Cb) < ((50+1)/2)*((38+1)/2) {
		t.Fatalf("shrunk to %v with %d chroma bytes", b, len(small.Cb))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, nil); err != nil {
		t.Fatal(err)
	}
}
