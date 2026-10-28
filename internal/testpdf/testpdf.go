// Package testpdf builds small synthetic PDFs for tests. It is only imported
// from _test files.
package testpdf

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
)

// JPEG returns a small deterministic gradient JPEG; different seeds differ.
func JPEG(seed, w, h int) []byte {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8(x*7 + y*3 + seed*37)})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 70}); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// Opts tweaks the generated structure.
type Opts struct {
	IndirectLength bool // image /Length as an indirect reference
	NestedTree     bool // page tree with intermediate /Pages nodes
	ExtraImage     int  // 1-based page that gets a second image (unsupported)

	WithContents bool           // every page gets a content stream that draws /Im0
	FormPage     int            // 1-based page that also lists a Form XObject /Fm0
	FormDrawn    bool           // ... and whose content stream invokes it
	UnusedImage  int            // 1-based page whose resources also list a second image /Im1
	Content      map[int]string // 1-based page -> its content stream (implies WithContents); /Im0 is the page image
	FormContent  string         // stream of the form /Fm0 (default draws a small square)
	PageAttrs    string         // appended to every page dict, e.g. "/MediaBox [0 0 612 792] /Rotate 90" (a later key wins)
	PageExtra    string         // appended to the content stream of FormPage (or page 1)
}

type builder struct {
	buf  bytes.Buffer
	offs map[int]int64
}

func newBuilder() *builder {
	b := &builder{offs: map[int]int64{}}
	b.buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	return b
}

func (b *builder) obj(num int, body string) {
	b.offs[num] = int64(b.buf.Len())
	fmt.Fprintf(&b.buf, "%d 0 obj\n%s\nendobj\n", num, body)
}

func (b *builder) stream(num int, dict string, data []byte, lengthRef int) {
	b.offs[num] = int64(b.buf.Len())
	l := fmt.Sprint(len(data))
	if lengthRef > 0 {
		l = fmt.Sprintf("%d 0 R", lengthRef)
	}
	fmt.Fprintf(&b.buf, "%d 0 obj\n<< %s /Length %s >>\nstream\n", num, dict, l)
	b.buf.Write(data)
	b.buf.WriteString("\nendstream\nendobj\n")
}

func imageDict(jpg []byte) string {
	return "/Type /XObject /Subtype /Image /Width 16 /Height 16 /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /DCTDecode"
}

// layout assigns object numbers: 1 catalog, 2 pages root, then per page a
// page object and an image object (and optional extras).
type layout struct {
	pageObj, imgObj []int
	next            int
	tree            []int // intermediate pages nodes when nested
}

func plan(n int, o Opts) layout {
	l := layout{next: 3}
	if o.NestedTree {
		l.tree = []int{l.next, l.next + 1}
		l.next += 2
	}
	for i := 0; i < n; i++ {
		l.pageObj = append(l.pageObj, l.next)
		l.imgObj = append(l.imgObj, l.next+1)
		l.next += 2
	}
	return l
}

func pageBody(parent, img int, extra bool) string {
	xo := fmt.Sprintf("/Im0 %d 0 R", img)
	if extra {
		xo += fmt.Sprintf(" /Im1 %d 0 R", img)
	}
	return fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 16 16] /Resources << /XObject << %s >> >> >>", parent, xo)
}

// pageBodyEx is pageBody plus an optional /Contents and /Fm0 form.
func pageBodyEx(parent, img, contents, form int, extraImg bool) string {
	xo := fmt.Sprintf("/Im0 %d 0 R", img)
	if extraImg {
		xo += fmt.Sprintf(" /Im1 %d 0 R", img)
	}
	if form > 0 {
		xo += fmt.Sprintf(" /Fm0 %d 0 R", form)
	}
	c := ""
	if contents > 0 {
		c = fmt.Sprintf(" /Contents %d 0 R", contents)
	}
	return fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 16 16]%s /Resources << /XObject << %s >> >> >>", parent, c, xo)
}

func (b *builder) writeTree(l layout, o Opts, n int) {
	b.obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	if o.NestedTree {
		half := n / 2
		kids := func(from, to int) string {
			s := ""
			for i := from; i < to; i++ {
				s += fmt.Sprintf("%d 0 R ", l.pageObj[i])
			}
			return s
		}
		b.obj(2, fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R %d 0 R] /Count %d >>", l.tree[0], l.tree[1], n))
		b.obj(l.tree[0], fmt.Sprintf("<< /Type /Pages /Parent 2 0 R /Kids [%s] /Count %d >>", kids(0, half), half))
		b.obj(l.tree[1], fmt.Sprintf("<< /Type /Pages /Parent 2 0 R /Kids [%s] /Count %d >>", kids(half, n), n-half))
	} else {
		kids := ""
		for i := 0; i < n; i++ {
			kids += fmt.Sprintf("%d 0 R ", l.pageObj[i])
		}
		b.obj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n))
	}
}

// Classic builds a PDF with a classic xref table.
func Classic(jpegs [][]byte, o Opts) []byte {
	n := len(jpegs)
	b := newBuilder()
	l := plan(n, o)
	b.writeTree(l, o, n)
	contentBase, formObj := 0, 0
	if o.WithContents || len(o.Content) > 0 {
		o.WithContents = true
		contentBase = l.next
		l.next += n
	}
	if o.FormPage > 0 {
		formObj = l.next
		l.next++
	}
	lenObj := l.next
	for i, j := range jpegs {
		parent := 2
		if o.NestedTree {
			parent = l.tree[0]
			if i >= n/2 {
				parent = l.tree[1]
			}
		}
		if o.WithContents || o.FormPage > 0 || o.UnusedImage > 0 || len(o.Content) > 0 {
			form, contents := 0, 0
			if o.FormPage == i+1 {
				form = formObj
			}
			if o.WithContents {
				contents = contentBase + i
				c := "q 16 0 0 16 0 0 cm /Im0 Do Q\n"
				if custom, ok := o.Content[i+1]; ok {
					c = custom
				}
				if form > 0 && o.FormDrawn {
					c += "/Fm0 Do\n"
				}
				if extraPage := max(o.FormPage, o.UnusedImage); extraPage == i+1 || (extraPage == 0 && i == 0) {
					c += o.PageExtra
				}
				b.stream(contents, "", []byte(c), 0)
			}
			body := pageBodyEx(parent, l.imgObj[i], contents, form, o.UnusedImage == i+1)
			if o.PageAttrs != "" {
				body = strings.Replace(body, "/MediaBox [0 0 16 16]", o.PageAttrs, 1)
			}
			b.obj(l.pageObj[i], body)
		} else {
			body := pageBody(parent, l.imgObj[i], o.ExtraImage == i+1)
			if o.PageAttrs != "" {
				body = strings.Replace(body, "/MediaBox [0 0 16 16]", o.PageAttrs, 1)
			}
			b.obj(l.pageObj[i], body)
		}
		ref := 0
		if o.IndirectLength {
			ref = lenObj + i
		}
		b.stream(l.imgObj[i], imageDict(j), j, ref)
	}
	if formObj > 0 {
		form := "0 g 0 0 4 4 re f"
		if o.FormContent != "" {
			form = o.FormContent
		}
		b.stream(formObj, "/Type /XObject /Subtype /Form /BBox [0 0 16 16]", []byte(form), 0)
	}
	size := l.next
	if o.IndirectLength {
		for i, j := range jpegs {
			b.obj(lenObj+i, fmt.Sprint(len(j)))
		}
		size += n
	}
	return b.finishTable(size, "")
}

func (b *builder) finishTable(size int, trailerExtra string) []byte {
	xrefAt := b.buf.Len()
	fmt.Fprintf(&b.buf, "xref\n0 %d\n0000000000 65535 f \n", size)
	for i := 1; i < size; i++ {
		if off, ok := b.offs[i]; ok {
			fmt.Fprintf(&b.buf, "%010d 00000 n \n", off)
		} else {
			b.buf.WriteString("0000000000 00000 f \n")
		}
	}
	fmt.Fprintf(&b.buf, "trailer\n<< /Size %d /Root 1 0 R %s>>\nstartxref\n%d\n%%%%EOF\n", size, trailerExtra, xrefAt)
	return b.buf.Bytes()
}

// WithUpdate appends an incremental update (classic xref, /Prev) that
// replaces the page tree root so only the first keep pages remain.
func WithUpdate(pdf []byte, jpegs [][]byte, keep int) []byte {
	l := plan(len(jpegs), Opts{})
	prev := lastStartxref(pdf)
	b := &builder{offs: map[int]int64{}}
	b.buf.Write(pdf)
	kids := ""
	for i := 0; i < keep; i++ {
		kids += fmt.Sprintf("%d 0 R ", l.pageObj[i])
	}
	b.obj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, keep))
	at := b.buf.Len()
	fmt.Fprintf(&b.buf, "xref\n2 1\n%010d 00000 n \n", b.offs[2])
	fmt.Fprintf(&b.buf, "trailer\n<< /Size %d /Root 1 0 R /Prev %d >>\nstartxref\n%d\n%%%%EOF\n", l.next, prev, at)
	return b.buf.Bytes()
}

func lastStartxref(pdf []byte) int64 {
	i := bytes.LastIndex(pdf, []byte("startxref"))
	var n int64
	fmt.Sscan(string(pdf[i+9:]), &n)
	return n
}

// XrefStream builds a PDF whose catalog, page tree and page objects live in a
// compressed object stream and whose xref is a PNG-predicted xref stream.
func XrefStream(jpegs [][]byte) []byte {
	n := len(jpegs)
	b := newBuilder()
	l := plan(n, Opts{})
	objstm := l.next
	xrefObj := l.next + 1
	// images first (plain stream objects)
	for i, j := range jpegs {
		b.stream(l.imgObj[i], imageDict(j), j, 0)
	}
	// compressed objects
	type co struct {
		num  int
		body string
	}
	kids := ""
	for i := 0; i < n; i++ {
		kids += fmt.Sprintf("%d 0 R ", l.pageObj[i])
	}
	objs := []co{
		{1, "<< /Type /Catalog /Pages 2 0 R >>"},
		{2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n)},
	}
	for i := 0; i < n; i++ {
		objs = append(objs, co{l.pageObj[i], pageBody(2, l.imgObj[i], false)})
	}
	var hdr, body bytes.Buffer
	for _, c := range objs {
		fmt.Fprintf(&hdr, "%d %d ", c.num, body.Len())
		body.WriteString(c.body + "\n")
	}
	var raw bytes.Buffer
	raw.Write(hdr.Bytes())
	raw.Write(body.Bytes())
	b.stream(objstm, fmt.Sprintf("/Type /ObjStm /N %d /First %d /Filter /FlateDecode", len(objs), hdr.Len()), deflate(raw.Bytes()), 0)

	// xref stream
	size := xrefObj + 1
	xrefAt := b.buf.Len()
	b.offs[xrefObj] = int64(xrefAt)
	var rows [][]byte
	row := func(t byte, a uint32, c uint16) []byte {
		r := make([]byte, 7)
		r[0] = t
		binary.BigEndian.PutUint32(r[1:], a)
		binary.BigEndian.PutUint16(r[5:], c)
		return r
	}
	for num := 0; num < size; num++ {
		switch {
		case num == 0:
			rows = append(rows, row(0, 0, 0xFFFF))
		case b.offs[num] != 0:
			rows = append(rows, row(1, uint32(b.offs[num]), 0))
		default:
			idx := -1
			for k, c := range objs {
				if c.num == num {
					idx = k
				}
			}
			if idx < 0 {
				rows = append(rows, row(0, 0, 0))
			} else {
				rows = append(rows, row(2, uint32(objstm), uint16(idx)))
			}
		}
	}
	b.stream(xrefObj, fmt.Sprintf("/Type /XRef /Size %d /W [1 4 2] /Root 1 0 R /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns 7 >>", size),
		deflate(pngUp(rows)), 0)
	fmt.Fprintf(&b.buf, "startxref\n%d\n%%%%EOF\n", xrefAt)
	return b.buf.Bytes()
}

func pngUp(rows [][]byte) []byte {
	var out []byte
	prev := make([]byte, len(rows[0]))
	for _, r := range rows {
		out = append(out, 2)
		for i := range r {
			out = append(out, r[i]-prev[i])
		}
		prev = r
	}
	return out
}

func deflate(b []byte) []byte {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	w.Write(b)
	w.Close()
	return out.Bytes()
}

// Deflate zlib-compresses b, as a /FlateDecode stream holds it.
func Deflate(b []byte) []byte { return deflate(b) }

// Raw describes a Flate-compressed raw-sample image. Rows are packed and
// padded to a byte boundary, like in a PDF image stream.
type Raw struct {
	W, H, Bpc  int
	ColorSpace string // PDF colour space, e.g. "/DeviceGray" or "[/Indexed /DeviceRGB 15 <...>]"
	Rows       []byte // H * rowBytes
	Mask       bool   // /ImageMask true (1-bit stencil; ColorSpace is ignored)
	Decode     string // optional /Decode array, e.g. "[1 0]"
	Filter     string // with Data: PDF filter name, e.g. "CCITTFaxDecode" (default FlateDecode)
	Parms      string // with Data: /DecodeParms dictionary body
	Data       []byte // pre-encoded stream data used instead of deflating Rows
}

// RawImage makes a deterministic image of the given shape.
func RawImage(seed, w, h, bpc int, cs string, colors int) Raw {
	rowBytes := (w*colors*bpc + 7) / 8
	rows := make([]byte, h*rowBytes)
	for i := range rows {
		rows[i] = byte(i*31 + seed*17 + i/rowBytes)
	}
	return Raw{W: w, H: h, Bpc: bpc, ColorSpace: cs, Rows: rows}
}

// ClassicRaw builds a classic-xref PDF with one Flate image per page.
func ClassicRaw(imgs []Raw) []byte {
	n := len(imgs)
	b := newBuilder()
	l := plan(n, Opts{})
	b.writeTree(l, Opts{}, n)
	for i, im := range imgs {
		b.obj(l.pageObj[i], pageBody(2, l.imgObj[i], false))
		d := fmt.Sprintf("/Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent %d /Filter /FlateDecode",
			im.W, im.H, im.ColorSpace, im.Bpc)
		if im.Mask {
			d = fmt.Sprintf("/Type /XObject /Subtype /Image /Width %d /Height %d /ImageMask true /BitsPerComponent 1 /Filter /FlateDecode", im.W, im.H)
		}
		data := deflate(im.Rows)
		if im.Data != nil {
			data = im.Data
			filt := "/" + strings.ReplaceAll(im.Filter, " ", " /") // "A B" becomes an array
			if strings.Contains(im.Filter, " ") {
				filt = "[" + filt + "]"
			}
			parms := ""
			if im.Parms != "" {
				parms = " /DecodeParms << " + im.Parms + " >>"
			}
			d = strings.Replace(d, "/Filter /FlateDecode", "/Filter "+filt+parms, 1)
		}
		if im.Decode != "" {
			d += " /Decode " + im.Decode
		}
		b.stream(l.imgObj[i], d, data, 0)
	}
	return b.finishTable(l.next, "")
}
