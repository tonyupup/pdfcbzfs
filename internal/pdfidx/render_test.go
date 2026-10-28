package pdfidx_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/pdfidx"
	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
)

func rjpegs(n int) [][]byte {
	var j [][]byte
	for i := 0; i < n; i++ {
		j = append(j, testpdf.JPEG(i, 16, 16))
	}
	return j
}

func openRender(t *testing.T, pdf []byte) *pdfidx.Index {
	t.Helper()
	x, err := pdfidx.OpenWith(bytes.NewReader(pdf), int64(len(pdf)), pdfidx.Options{Render: true})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestUnextractablePagesBecomeRenderPages(t *testing.T) {
	pdf := testpdf.Classic(rjpegs(4), testpdf.Opts{ExtraImage: 2})
	// without the option the document is rejected, as before
	if _, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf))); !errors.Is(err, pdfidx.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	x := openRender(t, pdf)
	if len(x.Pages) != 4 {
		t.Fatalf("%d pages", len(x.Pages))
	}
	for i, p := range x.Pages {
		if (p.Render != nil) != (i == 1) {
			t.Fatalf("page %d: render = %+v", i, p.Render)
		}
	}
	rp := x.Pages[1]
	if rp.Render.Index != 1 || rp.Ext != "jpg" || !strings.Contains(rp.Render.Reason, "exactly one XObject") {
		t.Fatalf("render page: %+v", rp)
	}
	if rp.Size < 48<<10 || rp.Size%1024 != 0 {
		t.Fatalf("budget %d should be at least 48 KiB and a whole number of KiB", rp.Size)
	}
	if n, first := x.RenderPages(); n != 1 || first != 1 {
		t.Fatalf("RenderPages = %d, %d", n, first)
	}
	if x.ReadParallelism() < 1 {
		t.Fatal("parallelism")
	}
	// extractable pages are untouched: the original JPEG bytes
	j := rjpegs(4)
	r := bytes.NewReader(pdf)
	for _, i := range []int{0, 2, 3} {
		b := make([]byte, x.Pages[i].Size)
		x.ReadImageAt(r, i, b, 0)
		if !bytes.Equal(b, j[i]) {
			t.Fatalf("page %d changed", i)
		}
	}
}

func TestRenderPageGeometry(t *testing.T) {
	for _, c := range []struct {
		name, attrs string
		w, h        int
	}{
		{"letter default", "/MediaBox [0 0 612 792]", 1275, 1650},
		{"rotated", "/MediaBox [0 0 612 792] /Rotate 90", 1650, 1275},
		{"rotated 270", "/MediaBox [0 0 612 792] /Rotate 270", 1650, 1275},
		{"rotated 180 keeps shape", "/MediaBox [0 0 612 792] /Rotate 180", 1275, 1650},
		{"cropbox", "/MediaBox [0 0 612 792] /CropBox [0 0 306 396]", 638, 825},
		{"cropbox larger than media is clipped", "/MediaBox [0 0 100 100] /CropBox [-50 -50 500 500]", 208, 208},
		{"huge page capped at the max side", "/MediaBox [0 0 2000 3000]", 1600, 2400},
		{"reversed corners", "/MediaBox [612 792 0 0]", 1275, 1650},
		{"user unit", "/MediaBox [0 0 306 396] /UserUnit 2", 1275, 1650},
		{"real numbers", "/MediaBox [0.0 0.0 595.276 841.89]", 1240, 1754},
	} {
		t.Run(c.name, func(t *testing.T) {
			pdf := testpdf.Classic(rjpegs(2), testpdf.Opts{ExtraImage: 1, PageAttrs: c.attrs})
			x := openRender(t, pdf)
			ri := x.Pages[0].Render
			if ri == nil || ri.W != c.w || ri.H != c.h {
				t.Fatalf("got %+v, want %dx%d", ri, c.w, c.h)
			}
		})
	}
}

func TestRenderBudgetGrowsWithPixels(t *testing.T) {
	small := openRender(t, testpdf.Classic(rjpegs(1), testpdf.Opts{ExtraImage: 1, PageAttrs: "/MediaBox [0 0 612 792]"})).Pages[0].Size
	big := openRender(t, testpdf.Classic(rjpegs(1), testpdf.Opts{ExtraImage: 1, PageAttrs: "/MediaBox [0 0 2000 3000]"})).Pages[0].Size
	if big <= small {
		t.Fatalf("budget %d for a big page, %d for a small one", big, small)
	}
}

func TestRenderPagesNeedAnIntactPageTree(t *testing.T) {
	pdf := breakPageObjects(testpdf.Classic(rjpegs(5), testpdf.Opts{ExtraImage: 2}), 4)
	// the renderer numbers pages itself; with a missing page object that numbering could drift
	_, err := pdfidx.OpenWith(bytes.NewReader(pdf), int64(len(pdf)), pdfidx.Options{Render: true})
	if !errors.Is(err, pdfidx.ErrUnsupported) || !strings.Contains(err.Error(), "missing page objects") {
		t.Fatalf("err = %v", err)
	}
	// without any page needing a render the old tolerance still applies
	pdf = breakPageObjects(testpdf.Classic(rjpegs(5), testpdf.Opts{}), 4)
	x, err := pdfidx.OpenWith(bytes.NewReader(pdf), int64(len(pdf)), pdfidx.Options{Render: true})
	if err != nil || len(x.Skipped) != 1 || len(x.Pages) != 4 {
		t.Fatalf("pages %v skipped %v err %v", x, x, err)
	}
}

func TestRenderedPageBytesComeFromTheRenderer(t *testing.T) {
	pdf := testpdf.Classic(rjpegs(3), testpdf.Opts{ExtraImage: 3})
	x := openRender(t, pdf)
	if _, err := x.RenderedPage(2); err == nil {
		t.Fatal("a render page without a renderer must fail, not return garbage")
	}
	size := x.Pages[2].Size
	var asked []int
	x.SetRenderer(func(page int) ([]byte, error) {
		asked = append(asked, page)
		b := make([]byte, size)
		for i := range b {
			b[i] = byte(i)
		}
		return b, nil
	})
	buf := make([]byte, 1000)
	n, err := x.ReadImageAt(bytes.NewReader(pdf), 2, buf, 5000)
	if err != nil || n != 1000 || buf[0] != byte(5000%256) || buf[999] != byte(5999%256) {
		t.Fatalf("n %d err %v first %d", n, err, buf[0])
	}
	x.ReadImageAt(bytes.NewReader(pdf), 2, buf, 0) // second read: served from the page cache
	if len(asked) != 1 || asked[0] != 2 {
		t.Fatalf("renderer asked %v, want the page once", asked)
	}
	if n, err := x.ReadImageAt(bytes.NewReader(pdf), 2, buf, size-10); n != 10 || err == nil {
		t.Fatalf("read at the end: n %d err %v", n, err)
	}
}

func TestRendererMustReturnTheBudgetSize(t *testing.T) {
	pdf := testpdf.Classic(rjpegs(2), testpdf.Opts{ExtraImage: 1})
	x := openRender(t, pdf)
	x.SetRenderer(func(int) ([]byte, error) { return make([]byte, 10), nil })
	if _, err := x.RenderedPage(0); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v", err)
	}
	x.SetRenderer(func(int) ([]byte, error) { return nil, errors.New("helper died") })
	if _, err := x.RenderedPage(0); err == nil || !strings.Contains(err.Error(), "helper died") {
		t.Fatalf("err = %v", err)
	}
}

func TestPasswordIsExposedForTheRenderer(t *testing.T) {
	for name, want := range map[string]string{
		"enc-aes-256.pdf":           "user-pw",
		"enc-rc4-128.pdf":           "user-pw",
		"enc-aes-128-emptyuser.pdf": "",
		"plain.pdf":                 "",
	} {
		pdf := fixture(t, name)
		x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)), []byte("user-pw"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(x.Password()) != want {
			t.Fatalf("%s: password %q, want %q", name, x.Password(), want)
		}
	}
}
