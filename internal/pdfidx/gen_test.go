package pdfidx_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
)

// TestGenFixtures writes testdata/plain.pdf, the input for the encrypted
// fixtures (see testdata/README.md). Run with PDFCBZFS_GEN_FIXTURES=1.
func TestGenFixtures(t *testing.T) {
	if os.Getenv("PDFCBZFS_GEN_FIXTURES") == "" {
		t.Skip("set PDFCBZFS_GEN_FIXTURES=1 to regenerate")
	}
	pdf := testpdf.Classic(jpegs(4), testpdf.Opts{})
	if err := os.WriteFile(filepath.Join("testdata", "plain.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	raw := testpdf.ClassicRaw([]testpdf.Raw{
		testpdf.RawImage(1, 33, 17, 4, "[/Indexed /DeviceRGB 15 <"+hexPalette(16)+">]", 1),
		testpdf.RawImage(2, 20, 11, 8, "/DeviceRGB", 3),
		testpdf.RawImage(3, 41, 9, 1, "/DeviceGray", 1),
	})
	if err := os.WriteFile(filepath.Join("testdata", "plain-raw.pdf"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
