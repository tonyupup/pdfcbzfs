package fusefs_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/fusefs"
	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
	"github.com/tonyupup/pdfcbzfs/internal/view"
)

func mountTest(t *testing.T, cfg view.Config) (lower, mnt string) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	lower, mnt = t.TempDir(), t.TempDir()
	cfg.Logf = t.Logf
	f, err := fusefs.New(lower, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := f.Mount(mnt, fusefs.MountOptions{})
	if err != nil {
		t.Skipf("cannot mount FUSE here: %v", err)
	}
	t.Cleanup(func() { srv.Unmount() })
	return lower, mnt
}

func jpegs(n int) [][]byte {
	var out [][]byte
	for i := 0; i < n; i++ {
		out = append(out, testpdf.JPEG(i, 16+i, 16))
	}
	return out
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func readZip(t *testing.T, path string) *zip.Reader {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return zr
}

func TestMirrorAndConvert(t *testing.T) {
	lower, mnt := mountTest(t, view.Config{PagesPerCBZ: 3, Mode: view.Auto})
	j := jpegs(7)
	os.WriteFile(filepath.Join(lower, "plain.txt"), []byte("hello"), 0o644)
	os.Mkdir(filepath.Join(lower, "sub"), 0o755)
	os.WriteFile(filepath.Join(lower, "sub", "short.pdf"), testpdf.Classic(j[:2], testpdf.Opts{}), 0o644)
	os.WriteFile(filepath.Join(lower, "long.pdf"), testpdf.Classic(j, testpdf.Opts{}), 0o644)
	os.WriteFile(filepath.Join(lower, "broken.pdf"), []byte("%PDF-1.4 but nothing else is valid here ........"), 0o644)

	got := names(t, mnt)
	want := []string{"broken.pdf", "long", "plain.txt", "sub"}
	if !equal(got, want) {
		t.Fatalf("root = %v, want %v", got, want)
	}
	if !equal(names(t, filepath.Join(mnt, "sub")), []string{"short.cbz"}) {
		t.Fatalf("sub = %v", names(t, filepath.Join(mnt, "sub")))
	}
	if !equal(names(t, filepath.Join(mnt, "long")), []string{"long v01.cbz", "long v02.cbz", "long v03.cbz"}) {
		t.Fatalf("long = %v", names(t, filepath.Join(mnt, "long")))
	}
	if b, _ := os.ReadFile(filepath.Join(mnt, "plain.txt")); string(b) != "hello" {
		t.Fatalf("plain.txt = %q", b)
	}
	if _, err := os.Stat(filepath.Join(mnt, "sub", "short.pdf")); !os.IsNotExist(err) {
		t.Fatalf("converted PDF must be hidden, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(mnt, "broken.pdf")); !bytes.HasPrefix(b, []byte("%PDF")) {
		t.Fatal("unconvertible PDF must pass through")
	}

	zr := readZip(t, filepath.Join(mnt, "sub", "short.cbz"))
	if len(zr.File) != 2 {
		t.Fatalf("short.cbz has %d entries", len(zr.File))
	}
	for i, f := range zr.File {
		rc, _ := f.Open()
		b, err := io.ReadAll(rc)
		if err != nil || !bytes.Equal(b, j[i]) {
			t.Fatalf("short.cbz/%s differs (err %v)", f.Name, err)
		}
	}
	z3 := readZip(t, filepath.Join(mnt, "long", "long v03.cbz"))
	if len(z3.File) != 2 { // pages 7 -> 3,3,1 plus ComicInfo = 2 entries in part 3
		t.Fatalf("part 3 has %d entries", len(z3.File))
	}
	if z3.File[0].Name != "ComicInfo.xml" {
		t.Fatalf("first entry %q", z3.File[0].Name)
	}
}

func TestWritesPassThrough(t *testing.T) {
	lower, mnt := mountTest(t, view.Config{})
	if err := os.WriteFile(filepath.Join(mnt, "new.cbz"), []byte("PK-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(lower, "new.cbz")); string(b) != "PK-data" {
		t.Fatalf("lower = %q", b)
	}
	if err := os.Mkdir(filepath.Join(mnt, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(mnt, "new.cbz"), filepath.Join(mnt, "d", "moved.cbz")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(lower, "d", "moved.cbz")); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedWithPasswordFile(t *testing.T) {
	lower, mnt := mountTest(t, view.Config{})
	src, err := os.ReadFile("../pdfidx/testdata/enc-aes-256.pdf")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(lower, "secret.pdf")
	os.WriteFile(p, src, 0o644)
	if !equal(names(t, mnt), []string{"secret.pdf", "secret.pdf.passwd"}[:1]) {
		t.Fatalf("without a password file the PDF is shown as-is: %v", names(t, mnt))
	}
	os.WriteFile(p+".passwd", []byte("user-pw\n"), 0o600)
	if !equal(names(t, mnt), []string{"secret.cbz", "secret.pdf.passwd"}) {
		t.Fatalf("with a password file it is converted: %v", names(t, mnt))
	}
	if zr := readZip(t, filepath.Join(mnt, "secret.cbz")); len(zr.File) != 4 {
		t.Fatalf("entries = %d", len(zr.File))
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFlatePDFBecomesPNGPages(t *testing.T) {
	lower, mnt := mountTest(t, view.Config{})
	pdf, err := os.ReadFile("../pdfidx/testdata/plain-raw.pdf")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(lower, "scan.pdf"), pdf, 0o644)
	zr := readZip(t, filepath.Join(mnt, "scan.cbz"))
	if len(zr.File) != 3 {
		t.Fatalf("entries = %d", len(zr.File))
	}
	for i, f := range zr.File {
		if want := fmt.Sprintf("000%d.png", i+1); f.Name != want {
			t.Fatalf("entry %d is %q, want %q", i, f.Name, want)
		}
		rc, _ := f.Open()
		if _, err := png.Decode(rc); err != nil {
			t.Fatalf("%s is not a valid PNG: %v", f.Name, err)
		}
		rc.Close()
	}
}

func TestFlatMode(t *testing.T) {
	lower, mnt := mountTest(t, view.Config{PagesPerCBZ: 3, Mode: view.Flat})
	j := jpegs(7)
	os.Mkdir(filepath.Join(lower, "sub"), 0o755)
	os.WriteFile(filepath.Join(lower, "sub", "short.pdf"), testpdf.Classic(j[:2], testpdf.Opts{}), 0o644)
	os.WriteFile(filepath.Join(lower, "long.pdf"), testpdf.Classic(j, testpdf.Opts{}), 0o644)

	want := []string{"long v01.cbz", "long v02.cbz", "long v03.cbz", "sub"}
	if got := names(t, mnt); !equal(got, want) {
		t.Fatalf("root = %v, want %v", got, want)
	}
	if got := names(t, filepath.Join(mnt, "sub")); !equal(got, []string{"short.cbz"}) {
		t.Fatalf("sub = %v", got)
	}
	if _, err := os.Stat(filepath.Join(mnt, "long")); !os.IsNotExist(err) {
		t.Fatalf("flat mode must not create a directory, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(mnt, "long v04.cbz")); !os.IsNotExist(err) {
		t.Fatalf("part beyond the last must not exist, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(mnt, "long.cbz")); !os.IsNotExist(err) {
		t.Fatalf("multi-part PDF must not also appear as long.cbz, got %v", err)
	}
	z3 := readZip(t, filepath.Join(mnt, "long v03.cbz"))
	if len(z3.File) != 2 || z3.File[0].Name != "ComicInfo.xml" {
		t.Fatalf("part 3 entries: %d, first %q", len(z3.File), z3.File[0].Name)
	}
	z1 := readZip(t, filepath.Join(mnt, "long v01.cbz"))
	rc, _ := z1.File[1].Open()
	if b, _ := io.ReadAll(rc); !bytes.Equal(b, j[0]) {
		t.Fatal("part 1 first page differs")
	}
}
