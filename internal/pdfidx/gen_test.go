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
}
