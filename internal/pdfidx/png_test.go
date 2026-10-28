package pdfidx_test

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonyupup/pdfcbzfs/internal/pdfidx"
	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
)

func hexPalette(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += fmt.Sprintf("%02x%02x%02x", byte(i*16), 255-byte(i*16), byte(i*53))
	}
	return s
}

// unpack returns the sample values of one row.
func unpack(rows []byte, rowBytes, y, w, bpc int) []int {
	row := rows[y*rowBytes : (y+1)*rowBytes]
	out := make([]int, w)
	for x := 0; x < w; x++ {
		bit := x * bpc
		v := int(row[bit/8]>>(8-bpc-bit%8)) & (1<<bpc - 1)
		out[x] = v
	}
	return out
}

func readPNG(t *testing.T, pdf []byte, x *pdfidx.Index, i int) image.Image {
	t.Helper()
	r := bytes.NewReader(pdf)
	b := make([]byte, x.Pages[i].Size)
	n, err := x.ReadImageAt(r, i, b, 0)
	if int64(n) != x.Pages[i].Size || (err != nil && err.Error() != "EOF") {
		t.Fatalf("page %d: n=%d size=%d err=%v", i, n, x.Pages[i].Size, err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("page %d is not a valid PNG: %v", i, err)
	}
	return img
}

func TestFlateImagesBecomePNG(t *testing.T) {
	cases := []struct {
		name   string
		w, h   int
		bpc    int
		cs     string
		colors int
	}{
		{"gray8", 37, 23, 8, "/DeviceGray", 1},
		{"gray1", 41, 9, 1, "/DeviceGray", 1},
		{"gray4", 19, 7, 4, "/DeviceGray", 1},
		{"rgb8", 20, 11, 8, "/DeviceRGB", 3},
		{"indexed4", 33, 17, 4, "[/Indexed /DeviceRGB 15 <" + hexPalette(16) + ">]", 1},
		{"indexed8", 16, 16, 8, "[/Indexed /DeviceRGB 255 <" + hexPalette(256) + ">]", 1},
		{"big (several stored blocks)", 300, 400, 8, "/DeviceGray", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			im := testpdf.RawImage(3, c.w, c.h, c.bpc, c.cs, c.colors)
			pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
			x := open(t, pdf)
			if x.Pages[0].Ext != "png" {
				t.Fatalf("ext = %q", x.Pages[0].Ext)
			}
			img := readPNG(t, pdf, x, 0)
			if img.Bounds().Dx() != c.w || img.Bounds().Dy() != c.h {
				t.Fatalf("size %v", img.Bounds())
			}
			rowBytes := (c.w*c.colors*c.bpc + 7) / 8
			for y := 0; y < c.h; y++ {
				if c.colors == 3 {
					for xx := 0; xx < c.w; xx++ {
						r, g, b, _ := img.At(xx, y).RGBA()
						o := y*rowBytes + xx*3
						if byte(r>>8) != im.Rows[o] || byte(g>>8) != im.Rows[o+1] || byte(b>>8) != im.Rows[o+2] {
							t.Fatalf("pixel (%d,%d) differs", xx, y)
						}
					}
					continue
				}
				want := unpack(im.Rows, rowBytes, y, c.w, c.bpc)
				for xx := 0; xx < c.w; xx++ {
					switch v := img.(type) {
					case *image.Paletted:
						if int(v.ColorIndexAt(xx, y)) != want[xx] {
							t.Fatalf("index (%d,%d) = %d, want %d", xx, y, v.ColorIndexAt(xx, y), want[xx])
						}
					default:
						g := color.GrayModel.Convert(img.At(xx, y)).(color.Gray).Y
						max := 1<<c.bpc - 1
						if int(g) != want[xx]*255/max {
							t.Fatalf("gray (%d,%d) = %d, want %d", xx, y, g, want[xx]*255/max)
						}
					}
				}
			}
			if c.cs[0] == '[' { // palette preserved
				p := img.(*image.Paletted).Palette
				r, g, b, _ := p[5].RGBA()
				if byte(r>>8) != 5*16 || byte(g>>8) != 255-5*16 || byte(b>>8) != byte((5*53)%256) {
					t.Fatalf("palette[5] = %v", p[5])
				}
			}
		})
	}
}

func TestPNGRandomAccessAndSize(t *testing.T) {
	im := testpdf.RawImage(1, 300, 400, 8, "/DeviceGray", 1)
	pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
	x := open(t, pdf)
	r := bytes.NewReader(pdf)
	full := make([]byte, x.Pages[0].Size)
	if _, err := x.ReadImageAt(r, 0, full, 0); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	n, _ := x.ReadImageAt(r, 0, buf, 70000)
	if n != 100 || !bytes.Equal(buf, full[70000:70100]) {
		t.Fatalf("mid read n=%d", n)
	}
}

func TestUnsupportedFlateImages(t *testing.T) {
	cases := map[string]string{
		"cmyk":    "/DeviceCMYK",
		"lab":     "[/Lab << >>]",
		"badicc":  "[/ICCBased 99 0 R]",
		"sepa":    "[/Separation /Spot /DeviceRGB << >>]",
		"nopaled": "[/Indexed /DeviceRGB 3 <00>]",
	}
	for name, cs := range cases {
		im := testpdf.RawImage(1, 8, 8, 8, cs, 1)
		pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
		_, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
		if !errors.Is(err, pdfidx.ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
	// RGB at 4 bits and 16-bit are not representable
	for _, bpc := range []int{4, 16} {
		im := testpdf.RawImage(1, 8, 8, bpc, "/DeviceRGB", 3)
		pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
		if _, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf))); !errors.Is(err, pdfidx.ErrUnsupported) {
			t.Errorf("rgb bpc=%d: err = %v", bpc, err)
		}
	}
}

func TestImageMaskBecomesGrayPNG(t *testing.T) {
	for _, c := range []struct {
		name, decode string
		invert       bool
	}{
		{"default", "", false},
		{"decode01", "[0 1]", false},
		{"decode10", "[1.0 0]", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			im := testpdf.RawImage(3, 45, 7, 1, "/DeviceGray", 1)
			im.Mask, im.Decode = true, c.decode
			pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
			x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
			if err != nil {
				t.Fatal(err)
			}
			if x.Pages[0].Ext != "png" {
				t.Fatalf("ext %q", x.Pages[0].Ext)
			}
			g, ok := readPNG(t, pdf, x, 0).(*image.Gray)
			if !ok {
				t.Fatal("not a gray image")
			}
			rb := (45 + 7) / 8
			for y := 0; y < 7; y++ {
				for x, v := range unpack(im.Rows, rb, y, 45, 1) {
					if c.invert {
						v ^= 1
					}
					// 1-bit gray: sample 0 is black, 1 is white
					if want := byte(v * 255); g.GrayAt(x, y).Y != want {
						t.Fatalf("(%d,%d) = %d, want %d", x, y, g.GrayAt(x, y).Y, want)
					}
				}
			}
		})
	}
}

func TestImageMaskOddDecodeUnsupported(t *testing.T) {
	im := testpdf.RawImage(3, 8, 8, 1, "/DeviceGray", 1)
	im.Mask, im.Decode = true, "[0 0.5]"
	pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
	if _, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf))); !errors.Is(err, pdfidx.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

const ccW, ccH = 203, 97

func ccittFixture(t *testing.T) (g4, raw []byte) {
	t.Helper()
	g4, err := os.ReadFile("testdata/ccitt-203x97.g4")
	if err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("testdata/ccitt-203x97.raw") // 1 = white, 0 = black
	if err != nil {
		t.Fatal(err)
	}
	return g4, raw
}

func checkGray1(t *testing.T, img image.Image, raw []byte, flip bool) {
	t.Helper()
	g, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("not a gray image: %T", img)
	}
	rb := (ccW + 7) / 8
	for y := 0; y < ccH; y++ {
		for x, v := range unpack(raw, rb, y, ccW, 1) {
			if flip {
				v ^= 1
			}
			if want := byte(v * 255); g.GrayAt(x, y).Y != want {
				t.Fatalf("(%d,%d) = %d, want %d", x, y, g.GrayAt(x, y).Y, want)
			}
		}
	}
}

func TestCCITTG4(t *testing.T) {
	g4, raw := ccittFixture(t)
	for _, c := range []struct {
		name         string
		mask         bool
		decode       string
		parms        string
		flip         bool // expected output differs from the decoded 1=white bitmap
		wantGrayFlip bool
	}{
		{"gray", false, "", "/K -1 /Columns 203 /Rows 97", false, false},
		{"mask", true, "", "/K -1 /Columns 203 /Rows 97", false, false},
		{"mask-decode10", true, "[1 0]", "/K -1 /Columns 203 /Rows 97", true, true},
		{"blackis1-mask-decode10", true, "[1 0]", "/K -1 /Columns 203 /Rows 97 /BlackIs1 true", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			im := testpdf.Raw{W: ccW, H: ccH, Bpc: 1, ColorSpace: "/DeviceGray", Mask: c.mask, Decode: c.decode,
				Filter: "CCITTFaxDecode", Parms: c.parms, Data: g4}
			pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
			x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
			if err != nil {
				t.Fatal(err)
			}
			checkGray1(t, readPNG(t, pdf, x, 0), raw, c.wantGrayFlip)
		})
	}
}

func TestCCITTRealPDF(t *testing.T) { // written by ImageMagick, an independent encoder
	pdf, err := os.ReadFile("testdata/ccitt-g4-gray.pdf")
	if err != nil {
		t.Fatal(err)
	}
	_, raw := ccittFixture(t)
	x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Pages) != 1 || x.Pages[0].Ext != "png" {
		t.Fatalf("pages: %+v", x.Pages)
	}
	checkGray1(t, readPNG(t, pdf, x, 0), raw, false)
}

func TestCCITTUnsupportedVariants(t *testing.T) {
	g4, _ := ccittFixture(t)
	for name, parms := range map[string]string{
		"group3":        "/K 0 /Columns 203",
		"default-k":     "/Columns 203",
		"columns-width": "/K -1 /Columns 100",
	} {
		im := testpdf.Raw{W: ccW, H: ccH, Bpc: 1, ColorSpace: "/DeviceGray", Filter: "CCITTFaxDecode", Parms: parms, Data: g4}
		pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
		if _, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf))); !errors.Is(err, pdfidx.ErrUnsupported) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCCITTTruncatedStreamPadsWhite(t *testing.T) {
	g4, _ := ccittFixture(t)
	im := testpdf.Raw{W: ccW, H: ccH, Bpc: 1, ColorSpace: "/DeviceGray", Mask: true, Filter: "CCITTFaxDecode",
		Parms: "/K -1 /Columns 203 /Rows 97", Data: g4[:len(g4)/2]}
	pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
	x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil {
		t.Fatal(err)
	}
	g := readPNG(t, pdf, x, 0).(*image.Gray)
	for xx := 0; xx < ccW; xx++ {
		if g.GrayAt(xx, ccH-1).Y != 255 {
			t.Fatalf("missing last row must be white, got %d at x=%d", g.GrayAt(xx, ccH-1).Y, xx)
		}
	}
}

func TestDecodeArrays(t *testing.T) {
	rgb := testpdf.RawImage(5, 20, 6, 8, "/DeviceRGB", 3)
	gray := testpdf.RawImage(6, 21, 5, 8, "/DeviceGray", 1)
	idx := testpdf.RawImage(7, 18, 4, 4, "[/Indexed /DeviceRGB 15 <"+hexPalette(16)+">]", 1)
	for _, c := range []struct {
		name   string
		im     testpdf.Raw
		decode string
		invert bool
		fail   string // substring of the expected error
	}{
		{"rgb identity", rgb, "[0 1 0 1 0 1]", false, ""},
		{"gray identity", gray, "[0.0 1.0]", false, ""},
		{"gray inverted", gray, "[1 0]", true, ""},
		{"rgb inverted", rgb, "[1 0 1 0 1 0]", true, ""},
		{"indexed identity", idx, "[0 15]", false, ""},
		{"indexed inverted", idx, "[15 0]", false, "/Decode [15 0]"},
		{"gray stretched", gray, "[0 0.5]", false, "/Decode [0 0.5]"},
		{"odd length", gray, "[0 1 0]", false, "/Decode"},
	} {
		t.Run(c.name, func(t *testing.T) {
			im := c.im
			im.Decode = c.decode
			pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
			x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
			if c.fail != "" {
				if !errors.Is(err, pdfidx.ErrUnsupported) || !strings.Contains(err.Error(), c.fail) {
					t.Fatalf("err = %v, want unsupported with %q", err, c.fail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// the PNG must equal the PDF's samples, flipped when inverted
			want := append([]byte{}, im.Rows...)
			if c.invert {
				for i := range want {
					want[i] = ^want[i]
				}
			}
			spec := testpdf.Raw{W: im.W, H: im.H, Bpc: im.Bpc, ColorSpace: im.ColorSpace, Rows: want}
			ref := testpdf.ClassicRaw([]testpdf.Raw{spec})
			xr, err := pdfidx.Open(bytes.NewReader(ref), int64(len(ref)))
			if err != nil {
				t.Fatal(err)
			}
			got, exp := readPNGBytes(t, pdf, x), readPNGBytes(t, ref, xr)
			if !bytes.Equal(got, exp) {
				t.Fatal("PNG differs from the expected samples")
			}
		})
	}
}

func readPNGBytes(t *testing.T, pdf []byte, x *pdfidx.Index) []byte {
	t.Helper()
	b := make([]byte, x.Pages[0].Size)
	if n, _ := x.ReadImageAt(bytes.NewReader(pdf), 0, b, 0); int64(n) != x.Pages[0].Size {
		t.Fatalf("short read %d of %d", n, x.Pages[0].Size)
	}
	return b
}

func TestInvertedDecodeOnJPEGIsUnsupported(t *testing.T) {
	im := testpdf.Raw{W: 16, H: 16, Bpc: 8, ColorSpace: "/DeviceGray", Decode: "[1 0]",
		Filter: "DCTDecode", Parms: "", Data: testpdf.JPEG(1, 16, 16)}
	pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
	if _, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf))); err == nil || !strings.Contains(err.Error(), "/Decode [1 0]") {
		t.Fatalf("err = %v", err)
	}
}

func TestZeroFilledTailIsExplained(t *testing.T) {
	pdf := make([]byte, 8192)
	copy(pdf, "%PDF-1.4\n")
	_, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
	if err == nil || !strings.Contains(err.Error(), "all zeros") {
		t.Fatalf("err = %v", err)
	}
}

func cmykPDF(t *testing.T, jpegFile, decode string, wrap bool) ([]byte, []byte) {
	t.Helper()
	jpg, err := os.ReadFile(filepath.Join("testdata", jpegFile))
	if err != nil {
		t.Fatal(err)
	}
	im := testpdf.Raw{W: 24, H: 16, Bpc: 8, ColorSpace: "/DeviceCMYK", Decode: decode, Filter: "DCTDecode", Data: jpg}
	if wrap {
		im.Filter, im.Data = "FlateDecode DCTDecode", testpdf.Deflate(jpg)
	}
	return testpdf.ClassicRaw([]testpdf.Raw{im}), jpg
}

func TestAdobeCMYKJPEGIsCopiedAsIs(t *testing.T) {
	for _, wrap := range []bool{false, true} {
		pdf, jpg := cmykPDF(t, "cmyk-adobe.jpg", "[1 0 1 0 1 0 1 0]", wrap)
		x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
		if err != nil {
			t.Fatalf("wrap=%v: %v", wrap, err)
		}
		if x.Pages[0].Ext != "jpg" || !bytes.Equal(readPNGBytes(t, pdf, x), jpg) {
			t.Fatalf("wrap=%v: page is not the original JPEG", wrap)
		}
	}
}

func TestCMYKJPEGWithoutAdobeInversionIsUnsupported(t *testing.T) {
	for name, c := range map[string]struct {
		file, decode, want string
	}{
		"no adobe marker": {"cmyk-noadobe.jpg", "[1 0 1 0 1 0 1 0]", "without an Adobe marker"},
		"no decode":       {"cmyk-adobe.jpg", "", "CMYK JPEG"},
		"identity decode": {"cmyk-adobe.jpg", "[0 1 0 1 0 1 0 1]", "CMYK JPEG"},
	} {
		for _, wrap := range []bool{false, true} {
			pdf, _ := cmykPDF(t, c.file, c.decode, wrap)
			_, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
			if !errors.Is(err, pdfidx.ErrUnsupported) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s wrap=%v: err = %v", name, wrap, err)
			}
		}
	}
}

func TestFlateWrappedJPEG(t *testing.T) {
	var jpgs [][]byte
	var imgs []testpdf.Raw
	for i := 0; i < 3; i++ {
		j := testpdf.JPEG(i, 16, 16)
		jpgs = append(jpgs, j)
		imgs = append(imgs, testpdf.Raw{W: 16, H: 16, Bpc: 8, ColorSpace: "/DeviceGray",
			Filter: "FlateDecode DCTDecode", Data: testpdf.Deflate(j)})
	}
	pdf := testpdf.ClassicRaw(imgs)
	x, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(pdf)
	for i := range jpgs {
		pg := x.Pages[i]
		if pg.Ext != "jpg" || pg.Size != int64(len(jpgs[i])) {
			t.Fatalf("page %d: ext %q size %d, want jpg %d", i, pg.Ext, pg.Size, len(jpgs[i]))
		}
		// read in two pieces at an offset: the inflated bytes must line up
		buf := make([]byte, pg.Size)
		half := len(buf) / 2
		n1, _ := x.ReadImageAt(r, i, buf[:half], 0)
		n2, _ := x.ReadImageAt(r, i, buf[half:], int64(half))
		if n1+n2 != len(buf) || !bytes.Equal(buf, jpgs[i]) {
			t.Fatalf("page %d: bytes differ", i)
		}
	}
}

func TestFlateStreamThatIsNotAJPEGIsUnsupported(t *testing.T) {
	im := testpdf.Raw{W: 16, H: 16, Bpc: 8, ColorSpace: "/DeviceGray", Filter: "FlateDecode DCTDecode",
		Data: testpdf.Deflate([]byte("definitely not a jpeg......."))}
	pdf := testpdf.ClassicRaw([]testpdf.Raw{im})
	if _, err := pdfidx.Open(bytes.NewReader(pdf), int64(len(pdf))); !errors.Is(err, pdfidx.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
