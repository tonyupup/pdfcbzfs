package pdfidx_test

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/pdfidx"
	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
)

// insertAfter puts n junk bytes after the first occurrence of marker, then
// fixes only the final startxref (the table itself moved with the bytes), so
// the xref entries of everything behind the insertion are n bytes too early:
// the "156 bytes inserted in the middle" damage of a real PDF.
func insertAfter(t *testing.T, pdf []byte, marker string, n int) []byte {
	t.Helper()
	i := bytes.Index(pdf, []byte(marker))
	if i < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	i += len(marker)
	junk := append([]byte("\n%"), bytes.Repeat([]byte("x"), n-3)...)
	junk = append(junk, '\n')
	out := append(append(append([]byte{}, pdf[:i]...), junk...), pdf[i:]...)
	re := regexp.MustCompile(`startxref\n(\d+)\n`)
	m := re.FindSubmatchIndex(out)
	v, _ := strconv.Atoi(string(out[m[2]:m[3]]))
	fixed := []byte(fmt.Sprintf("startxref\n%d\n", v+n))
	return append(append(append([]byte{}, out[:m[0]]...), fixed...), out[m[1]:]...)
}

func openAll(t *testing.T, pdf []byte) (*pdfidx.Index, [][]byte) {
	t.Helper()
	x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil {
		t.Fatal(err)
	}
	return x, pageBytes(t, pdf, x)
}

func TestXrefOffsetsShiftedByInsertedBytesAreRepaired(t *testing.T) {
	j := rjpegs(8)
	good := testpdf.Classic(j, testpdf.Opts{})
	for _, shift := range []int{16, 156, 1000, 5000} {
		pdf := insertAfter(t, good, "endobj\n3 0 obj", shift) // inserted early, so most objects are behind it
		x, got := openAll(t, pdf)
		if len(x.Pages) != 8 {
			t.Fatalf("shift %d: %d pages", shift, len(x.Pages))
		}
		for i := range j {
			if !bytes.Equal(got[i], j[i]) {
				t.Fatalf("shift %d: page %d differs", shift, i)
			}
		}
		if x.Repaired == 0 {
			t.Fatalf("shift %d: nothing reported as repaired", shift)
		}
	}
}

func TestAnUndamagedFileReportsNoRepairs(t *testing.T) {
	x, _ := openAll(t, testpdf.Classic(rjpegs(5), testpdf.Opts{}))
	if x.Repaired != 0 {
		t.Fatalf("Repaired = %d on a good file", x.Repaired)
	}
}

func TestOnlyObjectsBehindTheInsertionNeedRepair(t *testing.T) {
	// inserting late damages few offsets, inserting early damages many
	j := rjpegs(12)
	good := testpdf.Classic(j, testpdf.Opts{})
	late, early := 0, 0
	x, _ := openAll(t, insertAfter(t, good, "endobj\n20 0 obj", 40))
	late = x.Repaired
	x, _ = openAll(t, insertAfter(t, good, "endobj\n3 0 obj", 40))
	early = x.Repaired
	if !(early > late && late > 0) {
		t.Fatalf("repaired: late insertion %d, early insertion %d", late, early)
	}
}

func TestStartxrefPointingIntoAnObjectIsRepaired(t *testing.T) {
	good := testpdf.Classic(rjpegs(4), testpdf.Opts{})
	re := regexp.MustCompile(`startxref\n(\d+)\n`)
	m := re.FindSubmatchIndex(good)
	v, _ := strconv.Atoi(string(good[m[2]:m[3]]))
	for _, off := range []int{-30, -100, 7, 60} {
		bad := append([]byte{}, good...)
		copy(bad[m[2]:m[3]], []byte(fmt.Sprintf("%0*d", m[3]-m[2], v+off)))
		x, got := openAll(t, bad)
		if len(x.Pages) != 4 || !bytes.Equal(got[2], rjpegs(4)[2]) {
			t.Fatalf("offset %+d: pages %d", off, len(x.Pages))
		}
		if x.Repaired == 0 {
			t.Fatalf("offset %+d: not reported", off)
		}
	}
}

func TestPrevPointerThatIsOffByHundredsOfBytes(t *testing.T) {
	j := rjpegs(5)
	pdf := testpdf.WithUpdate(testpdf.Classic(j, testpdf.Opts{}), j, 3)
	re := regexp.MustCompile(`/Prev (\d+)`)
	m := re.FindSubmatchIndex(pdf)
	v, _ := strconv.Atoi(string(pdf[m[2]:m[3]]))
	bad := append([]byte{}, pdf...)
	copy(bad[m[2]:m[3]], []byte(fmt.Sprintf("%0*d", m[3]-m[2], v-156)))
	x, _ := openAll(t, bad)
	if len(x.Pages) != 3 || x.Repaired == 0 {
		t.Fatalf("pages %d repaired %d", len(x.Pages), x.Repaired)
	}
}

func TestAnObjectThatReallyIsGoneIsNotInvented(t *testing.T) {
	// repair only moves to where the object is; a missing one stays missing
	j := rjpegs(6)
	pdf := breakPageObjects(testpdf.Classic(j, testpdf.Opts{}), 3)
	x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil || len(x.Pages) != 5 || len(x.Skipped) != 1 {
		t.Fatalf("pages %d skipped %v err %v", len(x.Pages), x.Skipped, err)
	}
	// a pointer far outside the window is not followed either
	far := bytes.Replace(testpdf.Classic(j, testpdf.Opts{}), []byte("0000000015 00000 n"), []byte("0099999999 00000 n"), 1)
	if _, err := pdfidx.Open(bytes.NewReader(far), int64(len(far))); err != nil && !errors.Is(err, pdfidx.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestRepairHasALimit(t *testing.T) {
	// objects more than the search window away are not guessed at
	good := testpdf.Classic(rjpegs(8), testpdf.Opts{})
	pdf := insertAfter(t, good, "endobj\n3 0 obj", 200_000)
	if _, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf))); !errors.Is(err, pdfidx.ErrUnsupported) {
		t.Fatalf("a 200 KB shift must not be repaired, err = %v", err)
	}
}
