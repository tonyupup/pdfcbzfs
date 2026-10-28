package view_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
	"github.com/tonyupup/pdfcbzfs/internal/view"
)

func write(t *testing.T, dir, name string, b []byte) (string, *syscall.Stat_t) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		t.Fatal(err)
	}
	return p, &st
}

func pdf(n int) []byte {
	var j [][]byte
	for i := 0; i < n; i++ {
		j = append(j, testpdf.JPEG(i, 16, 16))
	}
	return testpdf.Classic(j, testpdf.Opts{})
}

func TestAutoSingleVersusFolder(t *testing.T) {
	dir := t.TempDir()
	c := view.NewCache(view.Config{PagesPerCBZ: 5, Mode: view.Auto})

	p, st := write(t, dir, "small.pdf", pdf(5))
	v := c.Probe(p, st)
	if v == nil || v.Folder || len(v.Parts) != 1 || v.SingleName() != "small.cbz" {
		t.Fatalf("5 pages with N=5 should be one in-place cbz: %+v", v)
	}

	p, st = write(t, dir, "Big Book.PDF", pdf(12))
	v = c.Probe(p, st)
	if v == nil || !v.Folder || len(v.Parts) != 3 {
		t.Fatalf("12 pages with N=5 should be 3 parts: %+v", v)
	}
	if v.DirName() != "Big Book" || v.PartName(0) != "Big Book v01.cbz" || v.PartName(2) != "Big Book v03.cbz" {
		t.Fatalf("names: %q %q %q", v.DirName(), v.PartName(0), v.PartName(2))
	}
	if v.DirIno()>>63 != 1 || v.PartIno(0) == v.PartIno(1) || v.DirIno() == v.PartIno(0) {
		t.Fatal("virtual inodes must be distinct with the top bit set")
	}
}

func TestFolderMode(t *testing.T) {
	dir := t.TempDir()
	c := view.NewCache(view.Config{PagesPerCBZ: 100, Mode: view.Folder})
	p, st := write(t, dir, "x.pdf", pdf(3))
	v := c.Probe(p, st)
	if v == nil || !v.Folder || len(v.Parts) != 1 || v.PartName(0) != "x v01.cbz" {
		t.Fatalf("%+v", v)
	}
}

func TestNotConvertibleIsNil(t *testing.T) {
	dir := t.TempDir()
	c := view.NewCache(view.Config{})
	p, st := write(t, dir, "bad.pdf", []byte("%PDF-1.4 definitely not a real file........"))
	if c.Probe(p, st) != nil {
		t.Fatal("garbage must pass through")
	}
}

func TestProbeCacheInvalidatesOnChange(t *testing.T) {
	dir := t.TempDir()
	c := view.NewCache(view.Config{PagesPerCBZ: 100})
	p, st := write(t, dir, "a.pdf", pdf(2))
	v1 := c.Probe(p, st)
	if v1 == nil || len(v1.Idx.Pages) != 2 {
		t.Fatal("first probe")
	}
	if c.Probe(p, st) != v1 {
		t.Fatal("unchanged file should hit the cache")
	}
	p, st = write(t, dir, "a.pdf", pdf(4))
	v2 := c.Probe(p, st)
	if v2 == nil || len(v2.Idx.Pages) != 4 {
		t.Fatal("changed file must be re-indexed")
	}
}

func TestNames(t *testing.T) {
	if s, ok := view.IsPDFName("a.b.PdF"); !ok || s != "a.b" {
		t.Fatal(s, ok)
	}
	if _, ok := view.IsPDFName("a.pdf.passwd"); ok {
		t.Fatal(".passwd must not look like a pdf")
	}
	if s, ok := view.IsCBZName("x.CBZ"); !ok || s != "x" {
		t.Fatal(s, ok)
	}
}

func TestFlatMode(t *testing.T) {
	dir := t.TempDir()
	c := view.NewCache(view.Config{PagesPerCBZ: 5, Mode: view.Flat})

	p, st := write(t, dir, "small.pdf", pdf(5))
	v := c.Probe(p, st)
	if v == nil || v.Folder || v.Flat || !v.Single() || len(v.Parts) != 1 {
		t.Fatalf("5 pages with N=5 should be one cbz: %+v", v)
	}

	p, st = write(t, dir, "Big Book.pdf", pdf(12))
	v = c.Probe(p, st)
	if v == nil || v.Folder || !v.Flat || v.Single() || len(v.Parts) != 3 {
		t.Fatalf("12 pages with N=5 should be 3 flat parts: %+v", v)
	}
	if v.FlatName(1) != "Big Book v02.cbz" {
		t.Fatalf("name %q", v.FlatName(1))
	}
}

func TestSplitPartName(t *testing.T) {
	for _, c := range []struct {
		in   string
		base string
		n    int
		ok   bool
	}{
		{"Book v01", "Book", 1, true},
		{"My v2 Book v12", "My v2 Book", 12, true},
		{"Book", "", 0, false},
		{"Book v", "", 0, false},
		{"Book vx1", "", 0, false},
		{"Book v+1", "", 0, false},
		{" v01", "", 0, false},
	} {
		b, n, ok := view.SplitPartName(c.in)
		if b != c.base || n != c.n || ok != c.ok {
			t.Errorf("%q = %q,%d,%v want %q,%d,%v", c.in, b, n, ok, c.base, c.n, c.ok)
		}
	}
}

func TestAria2PartialDownloadIsSkippedUntilFinished(t *testing.T) {
	dir := t.TempDir()
	var logs []string
	c := view.NewCache(view.Config{Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	p, st := write(t, dir, "dl.pdf", pdf(3))
	if err := os.WriteFile(p+".aria2", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c.Probe(p, st) != nil {
		t.Fatal("a file with a .aria2 control file is still downloading")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "download in progress") {
		t.Fatalf("logs = %q", logs)
	}
	os.Remove(p + ".aria2") // download finished; the PDF itself did not change
	if c.Probe(p, st) == nil {
		t.Fatal("must be probed once the control file is gone")
	}
}
