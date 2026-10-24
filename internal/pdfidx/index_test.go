package pdfidx_test

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/pdfidx"
	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
)

func jpegs(n int) [][]byte {
	var out [][]byte
	for i := 0; i < n; i++ {
		out = append(out, testpdf.JPEG(i, 16+i, 16))
	}
	return out
}

func open(t *testing.T, pdf []byte, pw ...[]byte) *pdfidx.Index {
	t.Helper()
	x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)), pw...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return x
}

func checkPages(t *testing.T, pdf []byte, x *pdfidx.Index, want [][]byte) {
	t.Helper()
	if len(x.Pages) != len(want) {
		t.Fatalf("pages = %d, want %d", len(x.Pages), len(want))
	}
	r := bytes.NewReader(pdf)
	for i, w := range want {
		got := make([]byte, len(w))
		n, err := x.ReadImageAt(r, i, got, 0)
		if n != len(w) || (err != nil && err.Error() != "EOF") {
			t.Fatalf("page %d: read %d bytes, err %v", i, n, err)
		}
		if !bytes.Equal(got, w) {
			t.Fatalf("page %d: bytes differ", i)
		}
		if x.Pages[i].Size != int64(len(w)) {
			t.Fatalf("page %d: size %d, want %d", i, x.Pages[i].Size, len(w))
		}
	}
}

func TestClassicXref(t *testing.T) {
	j := jpegs(5)
	pdf := testpdf.Classic(j, testpdf.Opts{})
	checkPages(t, pdf, open(t, pdf), j)
}

func TestIndirectLength(t *testing.T) {
	j := jpegs(4)
	pdf := testpdf.Classic(j, testpdf.Opts{IndirectLength: true})
	checkPages(t, pdf, open(t, pdf), j)
}

func TestNestedPageTree(t *testing.T) {
	j := jpegs(6)
	pdf := testpdf.Classic(j, testpdf.Opts{NestedTree: true})
	checkPages(t, pdf, open(t, pdf), j)
}

func TestXrefStreamAndObjectStreams(t *testing.T) {
	j := jpegs(7)
	pdf := testpdf.XrefStream(j)
	checkPages(t, pdf, open(t, pdf), j)
}

func TestIncrementalUpdate(t *testing.T) {
	j := jpegs(5)
	pdf := testpdf.WithUpdate(testpdf.Classic(j, testpdf.Opts{}), j, 3)
	checkPages(t, pdf, open(t, pdf), j[:3])
}

func TestRandomAccessRead(t *testing.T) {
	j := jpegs(3)
	pdf := testpdf.Classic(j, testpdf.Opts{})
	x := open(t, pdf)
	r := bytes.NewReader(pdf)
	w := j[1]
	buf := make([]byte, 10)
	n, err := x.ReadImageAt(r, 1, buf, int64(len(w))-4)
	if n != 4 || err == nil {
		t.Fatalf("tail read: n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf[:4], w[len(w)-4:]) {
		t.Fatal("tail bytes differ")
	}
	if n, err := x.ReadImageAt(r, 1, buf, int64(len(w))); n != 0 || err == nil {
		t.Fatalf("read at end: n=%d err=%v", n, err)
	}
}

func TestUnsupported(t *testing.T) {
	j := jpegs(3)
	cases := map[string][]byte{
		"two images on a page": testpdf.Classic(j, testpdf.Opts{ExtraImage: 2}),
		"truncated":            testpdf.Classic(j, testpdf.Opts{})[:200],
		"garbage":              bytes.Repeat([]byte("not a pdf at all\n"), 20),
	}
	for name, pdf := range cases {
		_, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
		if !errors.Is(err, pdfidx.ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
}

// TestRealPDF runs the indexer on an arbitrary local file, e.g.
//
//	PDFCBZFS_TEST_PDF=/path/to/book.pdf PDFCBZFS_TEST_PASSWORD=secret go test ./internal/pdfidx -run RealPDF -v
func TestRealPDF(t *testing.T) {
	path := os.Getenv("PDFCBZFS_TEST_PDF")
	if path == "" {
		t.Skip("PDFCBZFS_TEST_PDF not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	var pw [][]byte
	if p := os.Getenv("PDFCBZFS_TEST_PASSWORD"); p != "" {
		pw = append(pw, []byte(p))
	}
	x, err := pdfidx.Open(f, fi.Size(), pw...)
	if errors.Is(err, pdfidx.ErrUnsupported) {
		t.Skipf("not convertible, would be passed through unchanged: %v", err)
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Logf("%d pages, encrypted=%v", len(x.Pages), x.Encrypted())
	for _, i := range []int{0, len(x.Pages) / 2, len(x.Pages) - 1} {
		hdr := make([]byte, 3)
		if _, err := x.ReadImageAt(f, i, hdr, 0); err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if hdr[0] != 0xFF || hdr[1] != 0xD8 {
			t.Fatalf("page %d does not start with a JPEG SOI: % x", i, hdr)
		}
		t.Logf("page %d: %d bytes, JPEG ok", i, x.Pages[i].Size)
	}
}

func TestJunkAroundThePDF(t *testing.T) {
	var j [][]byte
	for i := 0; i < 3; i++ {
		j = append(j, testpdf.JPEG(i, 16+i, 16))
	}
	cases := map[string][]byte{
		"classic":     testpdf.Classic(j, testpdf.Opts{}),
		"xref-stream": testpdf.XrefStream(j),
		"nested-tree": testpdf.Classic(j, testpdf.Opts{NestedTree: true}),
		"incremental": testpdf.WithUpdate(testpdf.Classic(j, testpdf.Opts{}), j, 2),
	}
	for name, base := range cases {
		for variant, pdf := range map[string][]byte{
			"prefix":  append([]byte("JUNK\n\x00\x01 before the header\n"), base...),
			"padding": append(append([]byte{}, base...), make([]byte, 100<<10)...),
			"both":    append(append([]byte("\xef\xbb\xbfjunk"), base...), make([]byte, 5000)...),
		} {
			x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
			if err != nil {
				t.Fatalf("%s/%s: %v", name, variant, err)
			}
			r := bytes.NewReader(pdf)
			for i, pg := range x.Pages {
				buf := make([]byte, pg.Size)
				if n, _ := x.ReadImageAt(r, i, buf, 0); int64(n) != pg.Size || !bytes.Equal(buf, j[i]) {
					t.Fatalf("%s/%s page %d: image bytes differ", name, variant, i)
				}
			}
		}
	}
}

func TestUnsupportedLogSaysWhatThePageHolds(t *testing.T) {
	var j [][]byte
	for i := 0; i < 3; i++ {
		j = append(j, testpdf.JPEG(i, 16, 16))
	}
	pdf := testpdf.Classic(j, testpdf.Opts{ExtraImage: 2})
	_, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
	if err == nil || !strings.Contains(err.Error(), "found 2: Image=2") {
		t.Fatalf("err = %v", err)
	}
}

// breakPageObjects turns the "obj" keyword of the given 1-based pages into
// "xxx" (same length, so every xref offset stays valid): the xref entry then
// points at something that is not the object it names.
func breakPageObjects(pdf []byte, pages ...int) []byte {
	out := append([]byte{}, pdf...)
	var heads []int
	for from := 0; ; {
		i := bytes.Index(out[from:], []byte("/Type /Page "))
		if i < 0 {
			break
		}
		i += from
		heads = append(heads, bytes.LastIndex(out[:i], []byte("obj")))
		from = i + 1
	}
	for _, p := range pages {
		copy(out[heads[p-1]:], "xxx")
	}
	return out
}

func TestDanglingPagesAreSkipped(t *testing.T) {
	var j [][]byte
	for i := 0; i < 5; i++ {
		j = append(j, testpdf.JPEG(i, 16+i, 16))
	}
	pdf := breakPageObjects(testpdf.Classic(j, testpdf.Opts{}), 2, 5)
	x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Pages) != 3 || len(x.Skipped) != 2 {
		t.Fatalf("pages=%d skipped=%v", len(x.Pages), x.Skipped)
	}
	r := bytes.NewReader(pdf)
	for k, want := range []int{0, 2, 3} { // surviving pages, in order
		buf := make([]byte, x.Pages[k].Size)
		if n, _ := x.ReadImageAt(r, k, buf, 0); int64(n) != x.Pages[k].Size || !bytes.Equal(buf, j[want]) {
			t.Fatalf("page %d is not original page %d", k, want+1)
		}
	}
}

func TestTooManyDanglingPagesRejectsTheFile(t *testing.T) {
	var j [][]byte
	for i := 0; i < 12; i++ {
		j = append(j, testpdf.JPEG(i, 16, 16))
	}
	pdf := breakPageObjects(testpdf.Classic(j, testpdf.Opts{}), 1, 2, 3, 4, 5)
	if _, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf))); !errors.Is(err, pdfidx.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestUndefinedPageReferenceIsSkipped(t *testing.T) {
	var j [][]byte
	for i := 0; i < 3; i++ {
		j = append(j, testpdf.JPEG(i, 16, 16))
	}
	pdf := testpdf.Classic(j, testpdf.Opts{})
	// Point the first kid at an object number the file does not have, keeping
	// the length so the xref offsets stay valid.
	i := bytes.Index(pdf, []byte("/Kids [")) + len("/Kids [")
	k := i
	for pdf[k] >= '0' && pdf[k] <= '9' {
		k++
	}
	want := 0
	for n := i; n < k; n++ {
		pdf[n] = '9'
		want = want*10 + 9
	}
	x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Pages) != 2 || len(x.Skipped) != 1 || x.Skipped[0] != want {
		t.Fatalf("pages=%d skipped=%v want %d", len(x.Pages), x.Skipped, want)
	}
}
