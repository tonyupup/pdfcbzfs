package pdfidx_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/pdfidx"
	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
)

// The test page is 200x300 pt. The image fills x 10..190, y 20..290 (81% of the page).
const (
	pg200      = "/MediaBox [0 0 200 300]"
	bigImage   = "q 180 0 0 270 10 20 cm /Im0 Do Q\n"
	frame      = "0.8 g 0 0 200 8 re f 0.8 g 0 292 200 8 re f 0.8 g 0 0 8 300 re f 0.8 g 192 0 8 300 re f\n"
	whiteBg    = "1 g 0 0 200 300 re f\n"
	pageNumber = "BT /F1 9 Tf 95 3 Td (7) Tj ET\n"
)

func TestImageDominantPages(t *testing.T) {
	for _, c := range []struct {
		name    string
		opts    testpdf.Opts
		extract bool
		reason  string // substring of "not image-dominant: ..." when not extracted
	}{
		{"background, frames and a page number around the image",
			testpdf.Opts{Content: map[int]string{2: whiteBg + frame + bigImage + pageNumber}}, true, ""},
		{"only the image, placed large",
			testpdf.Opts{Content: map[int]string{2: bigImage}}, true, ""},
		{"an opaque background drawn under the image is hidden",
			testpdf.Opts{Content: map[int]string{2: "0 g 20 30 160 250 re f\n" + bigImage}}, true, ""},
		{"invisible OCR text over the image is free",
			testpdf.Opts{Content: map[int]string{2: bigImage + "BT 3 Tr /F1 12 Tf 50 150 Td (hello hello hello hello hello hello) Tj ET\n"}}, true, ""},
		{"text made only of spaces over the image draws nothing",
			testpdf.Opts{Content: map[int]string{2: bigImage + "BT /F1 12 Tf 50 150 Td (      ) Tj ET\n"}}, true, ""},
		{"an empty form over the image (an empty form field)",
			testpdf.Opts{FormPage: 2, FormContent: "q Q", Content: map[int]string{2: bigImage + "q 1 0 0 1 10 20 cm /Fm0 Do Q\n"}}, true, ""},
		{"a form holding only a page number, placed in the margin",
			testpdf.Opts{FormPage: 2, FormContent: "BT /F1 9 Tf 0 0 Td (7) Tj ET", Content: map[int]string{2: bigImage + "q 1 0 0 1 90 3 cm /Fm0 Do Q\n"}}, true, ""},

		{"nested cm: translate first, then scale (the order of the matrices matters)",
			testpdf.Opts{Content: map[int]string{2: frame + "q 1 0 0 1 10 20 cm 180 0 0 270 0 0 cm /Im0 Do Q\n" + pageNumber}}, true, ""},
		{"nested cm that moves the image out of the page",
			testpdf.Opts{Content: map[int]string{2: frame + "q 1 0 0 1 150 20 cm 180 0 0 270 0 0 cm /Im0 Do Q\n"}}, false, "image covers"},
		{"image too small a part of the page",
			testpdf.Opts{Content: map[int]string{2: frame + "q 140 0 0 200 30 50 cm /Im0 Do Q\n"}}, false, "image covers 47% of the page"},
		{"visible text over the image",
			testpdf.Opts{Content: map[int]string{2: bigImage + "BT /F1 12 Tf 50 150 Td (hello) Tj ET\n"}}, false, "drawn over the image"},
		{"a drawing over the image",
			testpdf.Opts{Content: map[int]string{2: bigImage + "1 0 0 rg 50 50 20 20 re f\n"}}, false, "drawn over the image"},
		{"the same drawing under the image is hidden, so fine",
			testpdf.Opts{Content: map[int]string{2: "1 0 0 rg 50 50 20 20 re f\n" + bigImage}}, true, ""},
		{"a form that paints over the image",
			testpdf.Opts{FormPage: 2, Content: map[int]string{2: bigImage + "q 1 0 0 1 20 40 cm /Fm0 Do Q\n"}}, false, "drawn over the image"},
		{"a lot of visible text in the margin",
			testpdf.Opts{Content: map[int]string{2: bigImage + "BT /F1 6 Tf 10 2 Td (aaaaaaaaaabbbbbbbbbbcccccccccc) Tj ET\n"}}, false, "30 bytes of visible text"},
		{"many vector paints",
			testpdf.Opts{Content: map[int]string{2: bigImage + strings.Repeat("0 g 0 0 1 1 re f\n", 100)}}, false, "100 vector paints"},
		{"the image is clipped smaller than itself",
			testpdf.Opts{Content: map[int]string{2: "q 50 50 100 100 re W n " + bigImage + "Q\n"}}, false, "cropped by a clip"},
		{"a flipped image",
			testpdf.Opts{Content: map[int]string{2: frame + "q 180 0 0 -270 10 290 cm /Im0 Do Q\n"}}, false, "rotated or flipped"},
		{"a rotated image",
			testpdf.Opts{Content: map[int]string{2: frame + "q 0 270 -180 0 190 20 cm /Im0 Do Q\n"}}, false, "rotated or flipped"},
		{"a rotated page",
			testpdf.Opts{PageAttrs: pg200 + " /Rotate 90", Content: map[int]string{2: frame + bigImage}}, false, "page is rotated"},
		{"two images",
			testpdf.Opts{UnusedImage: 2, Content: map[int]string{2: bigImage + "q 20 0 0 20 0 0 cm /Im1 Do Q\n"}}, false, "2 images are drawn"},
		{"a shading",
			testpdf.Opts{Content: map[int]string{2: bigImage + "/Sh0 sh\n"}}, false, "a shading is drawn"},
		{"an inline image",
			testpdf.Opts{Content: map[int]string{2: bigImage + "BI /W 1 /H 1 /BPC 8 /CS /G ID \x00 EI\n"}}, false, "content not understood"},
		{"an operator the interpreter does not know",
			testpdf.Opts{Content: map[int]string{2: bigImage + "1 2 3 frobnicate\n"}}, false, "content not understood"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// a page that lists a single XObject is extracted without looking at its
			// content (the older rule), so give the page a second, unused one
			if c.opts.FormPage == 0 && c.opts.UnusedImage == 0 {
				c.opts.UnusedImage = 2
			}
			if c.opts.PageAttrs == "" {
				c.opts.PageAttrs = pg200
			}
			j := rjpegs(3)
			pdf := testpdf.Classic(j, c.opts)
			x, err := pdfidx.OpenWith(bytes.NewReader(pdf), int64(len(pdf)), pdfidx.Options{Render: true})
			if err != nil {
				t.Fatal(err)
			}
			rp := x.Pages[1]
			if c.extract {
				if rp.Render != nil {
					t.Fatalf("must be extracted, but needs rendering: %s", rp.Render.Reason)
				}
				got := make([]byte, rp.Size)
				if n, _ := x.ReadImageAt(bytes.NewReader(pdf), 1, got, 0); int64(n) != rp.Size || !bytes.Equal(got, j[1]) {
					t.Fatal("the extracted page is not the page's image")
				}
				return
			}
			if rp.Render == nil {
				t.Fatal("must not be extracted: page content would be lost")
			}
			if !strings.Contains(rp.Render.Reason, "not image-dominant: ") || !strings.Contains(rp.Render.Reason, c.reason) {
				t.Fatalf("reason %q lacks %q", rp.Render.Reason, c.reason)
			}
			// other pages are untouched
			if x.Pages[0].Render != nil || x.Pages[2].Render != nil {
				t.Fatal("neighbouring pages must stay extractable")
			}
		})
	}
}

// A page whose content is only its image never needed the dominance rule: it is
// extracted whatever the size or placement (the long-standing rule).
func TestAPageThatDrawsOnlyItsImageIsStillExtractedAsBefore(t *testing.T) {
	for name, content := range map[string]string{
		"small":   "q 40 0 0 60 80 100 cm /Im0 Do Q\n",
		"flipped": "q 180 0 0 -270 10 290 cm /Im0 Do Q\n",
	} {
		j := rjpegs(3)
		pdf := testpdf.Classic(j, testpdf.Opts{PageAttrs: pg200, UnusedImage: 2, Content: map[int]string{2: content}})
		x, err := pdfidx.OpenWith(bytes.NewReader(pdf), int64(len(pdf)), pdfidx.Options{Render: true})
		if err != nil || x.Pages[1].Render != nil {
			t.Fatalf("%s: err %v render %+v", name, err, x.Pages[1].Render)
		}
	}
}

func TestImageDominantRuleNeedsAnImageAtAll(t *testing.T) {
	// a page of text and rectangles only has nothing to extract
	pdf := testpdf.Classic(rjpegs(3), testpdf.Opts{PageAttrs: pg200, UnusedImage: 2,
		Content: map[int]string{2: "BT /F1 12 Tf 10 100 Td (just text on this page) Tj ET\n"}})
	x, err := pdfidx.OpenWith(bytes.NewReader(pdf), int64(len(pdf)), pdfidx.Options{Render: true})
	if err != nil {
		t.Fatal(err)
	}
	if x.Pages[1].Render == nil || !strings.Contains(x.Pages[1].Render.Reason, "0 images are drawn") {
		t.Fatalf("%+v", x.Pages[1].Render)
	}
}

func TestOpenWithoutRenderStillRejectsWhatItCannotExtract(t *testing.T) {
	pdf := testpdf.Classic(rjpegs(3), testpdf.Opts{PageAttrs: pg200,
		Content: map[int]string{2: bigImage + "BT /F1 12 Tf 50 150 Td (hello) Tj ET\n"}, UnusedImage: 2})
	if _, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf))); err == nil || !strings.Contains(err.Error(), "not image-dominant") {
		t.Fatalf("err = %v", err)
	}
}
