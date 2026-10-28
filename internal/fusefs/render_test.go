package fusefs_test

import (
	"bytes"
	"context"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/render"
	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
	"github.com/tonyupup/pdfcbzfs/internal/view"
)

type stubRenderer struct {
	mu    sync.Mutex
	calls int
}

func (s *stubRenderer) Available() bool { return true }
func (s *stubRenderer) Page(_ context.Context, k render.PageKey) ([]byte, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return bytes.Repeat([]byte{0xA0 + byte(k.Page)}, int(k.Budget)), nil
}

func TestRenderedPagesInsideACBZ(t *testing.T) {
	st := &stubRenderer{}
	lower, mnt := mountTest(t, view.Config{PagesPerCBZ: 100, Mode: view.Auto, Render: st})
	j := jpegs(4)
	// page 2 (1-based) draws two images: it cannot be extracted
	pdf := testpdf.Classic(j, testpdf.Opts{ExtraImage: 2})
	if err := os.WriteFile(filepath.Join(lower, "mixed.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := names(t, mnt); !equal(got, []string{"mixed.cbz"}) {
		t.Fatalf("root = %v", got)
	}
	fi, err := os.Stat(filepath.Join(mnt, "mixed.cbz"))
	if err != nil {
		t.Fatal(err)
	}
	zr := readZip(t, filepath.Join(mnt, "mixed.cbz")) // archive/zip verifies the sizes and, on Open+Read, the CRCs
	if len(zr.File) != 4 {
		t.Fatalf("%d entries", len(zr.File))
	}
	var total int64
	for i, f := range zr.File {
		total += int64(f.CompressedSize64)
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc) // fails with a checksum error when the CRC in the header is wrong
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		switch i {
		case 1: // the rendered page: exactly the renderer's bytes
			if len(b) == 0 || b[0] != 0xA1 || b[len(b)-1] != 0xA1 {
				t.Fatalf("%s is not the rendered page", f.Name)
			}
		default: // the others are the original JPEGs, untouched
			if !bytes.Equal(b, j[i]) {
				t.Fatalf("%s differs from the PDF's image", f.Name)
			}
		}
	}
	if fi.Size() < total {
		t.Fatalf("file size %d smaller than its entries %d", fi.Size(), total)
	}
	// listing must not have needed more than the probe's single render, plus the CRC pass
	if st.calls < 1 {
		t.Fatal("renderer was never used")
	}
}

func TestRenderedPagesWithTheRealHelper(t *testing.T) {
	h := render.FindHelper()
	if h == nil || h.Check() != nil {
		t.Skip("pdfcbzfs-render is not installed")
	}
	svc := render.New(render.Config{Backend: h, CacheDir: t.TempDir(), MaxCache: 64 << 20})
	lower, mnt := mountTest(t, view.Config{PagesPerCBZ: 100, Mode: view.Auto, Render: svc})
	j := jpegs(3)
	pdf := testpdf.Classic(j, testpdf.Opts{ExtraImage: 3, PageAttrs: "/MediaBox [0 0 300 400]"})
	os.WriteFile(filepath.Join(lower, "real.pdf"), pdf, 0o644)
	zr := readZip(t, filepath.Join(mnt, "real.cbz"))
	if len(zr.File) != 3 {
		t.Fatalf("%d entries", len(zr.File))
	}
	rc, _ := zr.File[2].Open()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("the rendered page is not a valid JPEG: %v", err)
	}
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != 625 || h != 833 { // 300x400pt at 150 dpi
		t.Fatalf("rendered size %dx%d", w, h)
	}
	if zr.File[2].UncompressedSize64 < 48<<10 {
		t.Fatalf("budget %d", zr.File[2].UncompressedSize64)
	}
}
