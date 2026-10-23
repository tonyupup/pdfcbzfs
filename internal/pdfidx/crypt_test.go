package pdfidx_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/pdfidx"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pageBytes(t *testing.T, pdf []byte, x *pdfidx.Index) [][]byte {
	t.Helper()
	r := bytes.NewReader(pdf)
	var out [][]byte
	for i, p := range x.Pages {
		b := make([]byte, p.Size)
		n, err := x.ReadImageAt(r, i, b, 0)
		if int64(n) != p.Size || (err != nil && err.Error() != "EOF") {
			t.Fatalf("page %d: n=%d err=%v", i, n, err)
		}
		out = append(out, b)
	}
	return out
}

func TestEncryptedFixtures(t *testing.T) {
	plain := fixture(t, "plain.pdf")
	px := open(t, plain)
	want := pageBytes(t, plain, px)
	if len(want) != 4 {
		t.Fatalf("plain fixture has %d pages", len(want))
	}

	cases := []struct {
		file string
		pw   string // password to supply
		enc  bool
	}{
		{"enc-rc4-40.pdf", "user-pw", true},
		{"enc-rc4-128.pdf", "user-pw", true},
		{"enc-aes-128.pdf", "user-pw", true},
		{"enc-aes-256-r5.pdf", "user-pw", true},
		{"enc-aes-256.pdf", "user-pw", true},
		// the owner password opens the file too
		{"enc-rc4-128.pdf", "owner-pw", true},
		{"enc-aes-128.pdf", "owner-pw", true},
		{"enc-aes-256.pdf", "owner-pw", true},
		// empty user password: opens with no password supplied at all
		{"enc-aes-128-emptyuser.pdf", "", true},
		{"enc-aes-256-emptyuser.pdf", "", true},
	}
	for _, c := range cases {
		t.Run(c.file+"/"+c.pw, func(t *testing.T) {
			pdf := fixture(t, c.file)
			var pw [][]byte
			if c.pw != "" {
				pw = append(pw, []byte(c.pw))
			}
			x := open(t, pdf, pw...)
			if x.Encrypted() != c.enc {
				t.Fatalf("Encrypted() = %v", x.Encrypted())
			}
			got := pageBytes(t, pdf, x)
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("page %d: decrypted bytes differ from the plain PDF's JPEG", i)
				}
			}
		})
	}
}

func TestWrongOrMissingPassword(t *testing.T) {
	for _, f := range []string{"enc-rc4-40.pdf", "enc-rc4-128.pdf", "enc-aes-128.pdf", "enc-aes-256-r5.pdf", "enc-aes-256.pdf"} {
		pdf := fixture(t, f)
		for _, pw := range [][][]byte{nil, {[]byte("wrong")}} {
			_, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)), pw...)
			if !errors.Is(err, pdfidx.ErrPassword) || !errors.Is(err, pdfidx.ErrUnsupported) {
				t.Errorf("%s pw=%q: err = %v, want ErrPassword", f, pw, err)
			}
		}
	}
}

func TestSeveralCandidatePasswordsOneIsRight(t *testing.T) {
	pdf := fixture(t, "enc-aes-256.pdf")
	x := open(t, pdf, []byte("nope"), []byte("user-pw"))
	if len(x.Pages) != 4 {
		t.Fatalf("pages = %d", len(x.Pages))
	}
}

func TestEncryptedRandomAccess(t *testing.T) {
	plain := fixture(t, "plain.pdf")
	want := pageBytes(t, plain, open(t, plain))
	for _, f := range []string{"enc-rc4-128.pdf", "enc-aes-128.pdf", "enc-aes-256.pdf"} {
		pdf := fixture(t, f)
		x := open(t, pdf, []byte("user-pw"))
		r := bytes.NewReader(pdf)
		buf := make([]byte, 7)
		n, _ := x.ReadImageAt(r, 2, buf, 5)
		if n != 7 || !bytes.Equal(buf, want[2][5:12]) {
			t.Fatalf("%s: mid read n=%d", f, n)
		}
	}
}

// Encrypted Flate images (with an encrypted /Indexed palette string) must
// produce exactly the PNGs the unencrypted file produces.
func TestEncryptedFlateImagesBecomeSamePNG(t *testing.T) {
	plain := fixture(t, "plain-raw.pdf")
	want := pageBytes(t, plain, open(t, plain))
	if len(want) != 3 {
		t.Fatalf("plain-raw has %d pages", len(want))
	}
	for _, f := range []string{"encraw-rc4-128.pdf", "encraw-aes-128.pdf", "encraw-aes-256.pdf"} {
		t.Run(f, func(t *testing.T) {
			pdf := fixture(t, f)
			x := open(t, pdf, []byte("user-pw"))
			got := pageBytes(t, pdf, x)
			for i := range want {
				if x.Pages[i].Ext != "png" {
					t.Fatalf("page %d ext %q", i, x.Pages[i].Ext)
				}
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("page %d: PNG differs from the unencrypted file's PNG", i)
				}
			}
		})
	}
}
