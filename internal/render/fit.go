// Package render draws PDF pages that cannot be extracted (text, several
// images, JPEG 2000, ...) by running the pdfcbzfs-render helper, and turns the
// pixels into a JPEG of an exactly predetermined size.
package render

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
)

// Raster is a rendered page kept as planes: Y only when every pixel is gray,
// else 4:2:0 YCbCr (1.5 bytes per pixel, which is what the JPEG encoder wants
// anyway, so no full-size RGB copy ever exists).
type Raster struct {
	W, H   int
	Gray   bool
	Y      []byte
	Cb, Cr []byte
	CW, CH int // chroma plane size
}

// ErrTooBig means no quality or size made the page fit its budget.
var ErrTooBig = errors.New("render: page does not fit its byte budget")

// rasterBuilder turns RGB rows into a Raster.
type rasterBuilder struct {
	r        *Raster
	gray     bool
	cbs, crs []int32 // 2x2 chroma sums for the current pair of rows
	n        []int32
}

func newRasterBuilder(w, h int) *rasterBuilder {
	cw, ch := (w+1)/2, (h+1)/2
	return &rasterBuilder{
		r:    &Raster{W: w, H: h, Y: make([]byte, w*h), Cb: make([]byte, cw*ch), Cr: make([]byte, cw*ch), CW: cw, CH: ch},
		gray: true,
		cbs:  make([]int32, cw), crs: make([]int32, cw), n: make([]int32, cw),
	}
}

// addRow consumes row y (3 bytes per pixel); rows must come in order.
func (b *rasterBuilder) addRow(y int, rgb []byte) {
	r := b.r
	yrow := r.Y[y*r.W : (y+1)*r.W]
	for x := 0; x < r.W; x++ {
		pr, pg, pb := rgb[3*x], rgb[3*x+1], rgb[3*x+2]
		if pr != pg || pg != pb {
			b.gray = false
		}
		yy, cb, cr := color.RGBToYCbCr(pr, pg, pb)
		yrow[x] = yy
		c := x / 2
		b.cbs[c] += int32(cb)
		b.crs[c] += int32(cr)
		b.n[c]++
	}
	if y%2 == 1 || y == r.H-1 {
		base := (y / 2) * r.CW
		for c := 0; c < r.CW; c++ {
			r.Cb[base+c] = byte(b.cbs[c] / b.n[c])
			r.Cr[base+c] = byte(b.crs[c] / b.n[c])
			b.cbs[c], b.crs[c], b.n[c] = 0, 0, 0
		}
	}
}

func (b *rasterBuilder) finish() *Raster {
	if b.gray {
		b.r.Gray, b.r.Cb, b.r.Cr = true, nil, nil
	}
	return b.r
}

// image converts the raster for the JPEG encoder (no copy).
func (r *Raster) image() image.Image {
	if r.Gray {
		return &image.Gray{Pix: r.Y, Stride: r.W, Rect: image.Rect(0, 0, r.W, r.H)}
	}
	return &image.YCbCr{Y: r.Y, Cb: r.Cb, Cr: r.Cr, YStride: r.W, CStride: r.CW,
		SubsampleRatio: image.YCbCrSubsampleRatio420, Rect: image.Rect(0, 0, r.W, r.H)}
}

// FitJPEG encodes the page as a JPEG of exactly budget bytes. It lowers the
// quality until the data fits, then shrinks the page if even the lowest
// quality is too big, and pads the remainder with COM (comment) segments, so
// the result stays a valid JPEG that any viewer shows normally.
func FitJPEG(r *Raster, budget int64) ([]byte, error) {
	if r.W < 1 || r.H < 1 {
		return nil, errors.New("render: empty page")
	}
	img := r.image()
	var buf bytes.Buffer
	buf.Grow(int(min(budget, 8<<20)) + 4096) // no repeated doubling garbage
	for step := 0; step < 6; step++ {
		for q := 82; q >= 35; q -= 7 {
			buf.Reset()
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
				return nil, err
			}
			if int64(buf.Len()) <= budget {
				return pad(buf.Bytes(), int(budget)), nil
			}
		}
		img = shrink(img, 0.85)
	}
	return nil, ErrTooBig
}

// pad grows a JPEG to exactly n bytes. Padding of 4 bytes or more goes into
// COM segments right after SOI; 1 to 3 leftover bytes are zeros after EOI,
// which decoders ignore.
func pad(jpg []byte, n int) []byte {
	extra := n - len(jpg)
	if extra <= 0 {
		return jpg
	}
	out := make([]byte, 0, n)
	out = append(out, jpg[:2]...) // SOI
	tail := 0
	if extra < 4 {
		tail, extra = extra, 0
	}
	for extra > 0 {
		seg := min(extra, 65537) // marker(2) + length(2) + up to 65533 payload
		if rest := extra - seg; rest > 0 && rest < 4 {
			seg -= 4 // never leave a remainder too small for a segment
		}
		out = append(out, 0xFF, 0xFE, byte((seg-2)>>8), byte(seg-2))
		out = append(out, make([]byte, seg-4)...)
		extra -= seg
	}
	out = append(out, jpg[2:]...)
	return append(out, make([]byte, tail)...)
}

// shrink scales an image by f using a box average (only a fallback for pages
// that cannot meet their budget at the lowest quality).
func shrink(src image.Image, f float64) image.Image {
	b := src.Bounds()
	w, h := max(1, int(float64(b.Dx())*f)), max(1, int(float64(b.Dy())*f))
	switch s := src.(type) {
	case *image.Gray:
		d := image.NewGray(image.Rect(0, 0, w, h))
		scaleInto(w, h, b.Dx(), b.Dy(), 1, s.Pix, s.Stride, d.Pix, d.Stride)
		return d
	case *image.YCbCr:
		d := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
		scaleInto(w, h, b.Dx(), b.Dy(), 1, s.Y, s.YStride, d.Y, d.YStride)
		scw, sch := (b.Dx()+1)/2, (b.Dy()+1)/2
		dcw, dch := (w+1)/2, (h+1)/2
		scaleInto(dcw, dch, scw, sch, 1, s.Cb, s.CStride, d.Cb, d.CStride)
		scaleInto(dcw, dch, scw, sch, 1, s.Cr, s.CStride, d.Cr, d.CStride)
		return d
	}
	return src
}

func scaleInto(w, h, sw, sh, bpp int, src []byte, sstride int, dst []byte, dstride int) {
	for y := 0; y < h; y++ {
		y0, y1 := y*sh/h, max(y*sh/h+1, (y+1)*sh/h)
		for x := 0; x < w; x++ {
			x0, x1 := x*sw/w, max(x*sw/w+1, (x+1)*sw/w)
			for c := 0; c < bpp; c++ {
				sum, n := 0, 0
				for yy := y0; yy < y1; yy++ {
					for xx := x0; xx < x1; xx++ {
						sum += int(src[yy*sstride+xx*bpp+c])
						n++
					}
				}
				dst[y*dstride+x*bpp+c] = byte(sum / n)
			}
		}
	}
}
