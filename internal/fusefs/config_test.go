package fusefs_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
	"github.com/tonyupup/pdfcbzfs/internal/view"
)

func TestConfigFilesInAnyDirectory(t *testing.T) {
	lower, mnt := mountTest(t, view.Config{PagesPerCBZ: 100, Mode: view.Auto})
	j := jpegs(2)
	book := testpdf.Classic(j, testpdf.Opts{})
	os.MkdirAll(filepath.Join(lower, "课本", "二年级"), 0o755)
	os.MkdirAll(filepath.Join(lower, "漫画"), 0o755)
	for _, p := range []string{"top.pdf", "draft-top.pdf", "课本/a.pdf", "课本/二年级/b.pdf", "漫画/draft-c.pdf", "漫画/d.pdf"} {
		os.WriteFile(filepath.Join(lower, p), book, 0o644)
	}
	// one file at the root (wildcard, applies everywhere) and one in a subdirectory
	os.WriteFile(filepath.Join(lower, ".pdf2cbz.conf"), []byte("[ignore]\ndraft-*.pdf\n"), 0o644)
	os.WriteFile(filepath.Join(lower, "课本", ".pdf2cbz.conf"), []byte("[ignore]\n二年级/\n"), 0o644)

	check := func(dir string, want []string) {
		t.Helper()
		if got := names(t, filepath.Join(mnt, dir)); !equal(got, want) {
			t.Fatalf("%q = %v, want %v", dir, got, want)
		}
	}
	check("", []string{".pdf2cbz.conf", "draft-top.pdf", "top.cbz", "漫画", "课本"})
	check("课本", []string{".pdf2cbz.conf", "a.cbz", "二年级"})
	check("课本/二年级", []string{"b.pdf"})
	check("漫画", []string{"d.cbz", "draft-c.pdf"})

	// the excluded PDF is the real file, byte for byte
	if b, _ := os.ReadFile(filepath.Join(mnt, "draft-top.pdf")); !bytes.Equal(b, book) {
		t.Fatal("an excluded PDF must pass through unchanged")
	}

	// editing the file through the mount takes effect without a restart
	if err := os.WriteFile(filepath.Join(mnt, ".pdf2cbz.conf"), []byte("[ignore]\n# nothing excluded any more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(mnt, "draft-top.cbz")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the edit was not picked up: %v", names(t, mnt))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestDefaultPasswordsFromConfig(t *testing.T) {
	lower, mnt := mountTest(t, view.Config{PagesPerCBZ: 100, Mode: view.Auto})
	enc, err := os.ReadFile(filepath.Join("..", "pdfidx", "testdata", "enc-aes-256.pdf")) // password user-pw
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(lower, "scans"), 0o755)
	os.WriteFile(filepath.Join(lower, "scans", "secret one.pdf"), enc, 0o644)
	os.WriteFile(filepath.Join(lower, "other.pdf"), enc, 0o644)
	os.WriteFile(filepath.Join(lower, ".pdf2cbz.conf"), []byte("[passwords]\nscans/*.pdf = user-pw\n"), 0o644)
	if got := names(t, filepath.Join(mnt, "scans")); !equal(got, []string{"secret one.cbz"}) {
		t.Fatalf("scans = %v", got)
	}
	if got := names(t, mnt); !equal(got, []string{".pdf2cbz.conf", "other.pdf", "scans"}) {
		t.Fatalf("root = %v (other.pdf has no password rule and must stay a PDF)", got)
	}
	if zr := readZip(t, filepath.Join(mnt, "scans", "secret one.cbz")); len(zr.File) != 4 {
		t.Fatalf("%d pages", len(zr.File))
	}
}
