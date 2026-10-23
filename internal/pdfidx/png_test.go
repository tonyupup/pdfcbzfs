package pdfidx_test

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
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
