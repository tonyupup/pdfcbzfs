package pdfidx

import (
	"fmt"
	"math"
)

// An image-dominant page is a page whose content is one image that fills most
// of the page, plus furniture: a white background, frames or rules in the
// margins, a page number. Word and InDesign exports of comics look like this.
// Extracting the image gives a clean page; the furniture is dropped on
// purpose. The check is deliberately conservative, because it loosens the
// rule that a page must be nothing but an image:
//
//   - exactly one image is drawn, unrotated and unflipped, and it covers at
//     least dominantMinArea of the visible page;
//   - nothing visible is painted over it after it was drawn (what comes before
//     is hidden by the opaque image or lies outside it);
//   - it is not cropped by a clip smaller than itself;
//   - the rest is little: at most maxFurnitureGlyphs bytes of visible text
//     (invisible OCR text is free), at most maxFurniturePaints path paints, no
//     shadings, no inline images.
const (
	dominantMinArea    = 0.70
	maxFurnitureGlyphs = 12
	maxFurniturePaints = 80
	layoutMaxFormDepth = 4
	layoutEps          = 0.5 // points of tolerance when comparing rectangles
)

type mat [6]float64 // a b c d e f, row-vector convention as in PDF

var ident = mat{1, 0, 0, 1, 0, 0}

// then applies m first and n second.
func (m mat) then(n mat) mat {
	return mat{
		m[0]*n[0] + m[1]*n[2], m[0]*n[1] + m[1]*n[3],
		m[2]*n[0] + m[3]*n[2], m[2]*n[1] + m[3]*n[3],
		m[4]*n[0] + m[5]*n[2] + n[4], m[4]*n[1] + m[5]*n[3] + n[5],
	}
}

func (m mat) pt(x, y float64) (float64, float64) {
	return m[0]*x + m[2]*y + m[4], m[1]*x + m[3]*y + m[5]
}

type rect struct{ x0, y0, x1, y1 float64 }

func (r rect) area() float64 { return max(0, r.x1-r.x0) * max(0, r.y1-r.y0) }

func (r rect) intersect(o rect) rect {
	return rect{max(r.x0, o.x0), max(r.y0, o.y0), min(r.x1, o.x1), min(r.y1, o.y1)}
}

// overlaps reports whether the interiors overlap by more than the tolerance.
func (r rect) overlaps(o rect) bool {
	return min(r.x1, o.x1)-max(r.x0, o.x0) > layoutEps && min(r.y1, o.y1)-max(r.y0, o.y0) > layoutEps
}

// contains reports whether o lies within r (with tolerance).
func (r rect) contains(o rect) bool {
	return o.x0 >= r.x0-layoutEps && o.y0 >= r.y0-layoutEps && o.x1 <= r.x1+layoutEps && o.y1 <= r.y1+layoutEps
}

func boundsOf(m mat, pts [][2]float64) rect {
	r := rect{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for _, p := range pts {
		x, y := m.pt(p[0], p[1])
		r = rect{min(r.x0, x), min(r.y0, y), max(r.x1, x), max(r.y1, y)}
	}
	return r
}

type paint struct {
	kind   byte // 'p' path, 't' visible text, 's' shading
	r      rect
	glyphs int
	order  int
}

type imageUse struct {
	name    string
	val     any // the page's reference to the image XObject
	ctm     mat
	r       rect
	clip    rect
	hasClip bool
	order   int
}

type layout struct {
	paints []paint
	images []imageUse
	order  int
}

type gstate struct {
	ctm      mat
	clip     rect
	hasClip  bool
	textMode int64
	fontSize float64
}

type interp struct {
	w      *walker
	l      *layout
	nforms int
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func nums(args []any, n int) ([]float64, bool) {
	if len(args) < n {
		return nil, false
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		f, ok := num(args[len(args)-n+i])
		if !ok {
			return nil, false
		}
		out[i] = f
	}
	return out, true
}

// glyphBytes counts the bytes of a shown string that are neither NUL nor a
// space: those carry no visible glyph.
func glyphBytes(s []byte) int {
	n := 0
	for _, c := range s {
		if c != 0 && c != ' ' {
			n++
		}
	}
	return n
}

// nonPaintingOps change state without putting ink on the page.
var nonPaintingOps = map[string]bool{
	"gs": true, "w": true, "J": true, "j": true, "M": true, "d": true, "ri": true, "i": true,
	"g": true, "G": true, "rg": true, "RG": true, "k": true, "K": true,
	"cs": true, "CS": true, "sc": true, "SC": true, "scn": true, "SCN": true,
	"BMC": true, "BDC": true, "EMC": true, "MP": true, "DP": true, "BX": true, "EX": true,
	"Tc": true, "Tw": true, "Tz": true, "Ts": true, "d0": true, "d1": true,
}

// run interprets one content stream (a page or a form).
func (it *interp) run(content []byte, res dict, st gstate, depth int) error {
	p := &parser{b: append(append([]byte{}, content...), ' ')}
	var args []any
	var stack []gstate
	var path rect
	havePath, pendingClip := false, false
	path = rect{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	var tm, tlm mat = ident, ident
	leading := 0.0

	addPt := func(x, y float64) {
		px, py := st.ctm.pt(x, y)
		path = rect{min(path.x0, px), min(path.y0, py), max(path.x1, px), max(path.y1, py)}
		havePath = true
	}
	record := func(kind byte, r rect, glyphs int) {
		it.l.order++
		it.l.paints = append(it.l.paints, paint{kind: kind, r: r, glyphs: glyphs, order: it.l.order})
	}
	endPath := func(paintIt bool) {
		if havePath {
			if paintIt {
				record('p', path, 0)
			}
			if pendingClip {
				if st.hasClip {
					st.clip = st.clip.intersect(path)
				} else {
					st.clip, st.hasClip = path, true
				}
			}
		}
		havePath, pendingClip = false, false
		path = rect{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	}
	show := func(s []byte) {
		if st.textMode == 3 || st.textMode == 7 || len(s) == 0 {
			return // invisible text, such as an OCR layer
		}
		g := glyphBytes(s)
		if g == 0 {
			return
		}
		em := float64(len(s)) * 0.6
		fs := st.fontSize
		if fs == 0 {
			fs = 12
		}
		m := mat{fs, 0, 0, fs, 0, 0}.then(tm).then(st.ctm)
		record('t', boundsOf(m, [][2]float64{{0, -0.25}, {em, -0.25}, {0, 1}, {em, 1}}), g)
	}

	for {
		p.skipWS()
		if p.pos >= len(p.b)-1 {
			return nil
		}
		o, err := p.parseObject()
		if err != nil {
			return err
		}
		kw, isKw := o.(keyword)
		if !isKw {
			args = append(args, o)
			if len(args) > 96 {
				args = args[len(args)-96:]
			}
			continue
		}
		op := string(kw)
		switch op {
		case "q":
			stack = append(stack, st)
			if len(stack) > 64 {
				return fmt.Errorf("graphics state nesting")
			}
		case "Q":
			if n := len(stack); n > 0 {
				st, stack = stack[n-1], stack[:n-1]
			}
		case "cm":
			f, ok := nums(args, 6)
			if !ok {
				return fmt.Errorf("bad cm")
			}
			st.ctm = mat{f[0], f[1], f[2], f[3], f[4], f[5]}.then(st.ctm)
		case "m", "l":
			if f, ok := nums(args, 2); ok {
				addPt(f[0], f[1])
			}
		case "c":
			if f, ok := nums(args, 6); ok {
				addPt(f[0], f[1])
				addPt(f[2], f[3])
				addPt(f[4], f[5])
			}
		case "v", "y":
			if f, ok := nums(args, 4); ok {
				addPt(f[0], f[1])
				addPt(f[2], f[3])
			}
		case "re":
			if f, ok := nums(args, 4); ok {
				addPt(f[0], f[1])
				addPt(f[0]+f[2], f[1]+f[3])
			}
		case "h":
		case "W", "W*":
			pendingClip = true
		case "n":
			endPath(false)
		case "f", "F", "f*", "B", "B*", "b", "b*", "S", "s":
			endPath(true)
		case "sh":
			full := st.clip
			if !st.hasClip {
				full = rect{math.Inf(-1), math.Inf(-1), math.Inf(1), math.Inf(1)}
			}
			record('s', full, 0)
		case "BT":
			tm, tlm = ident, ident
		case "ET":
		case "Tf":
			if len(args) >= 1 {
				if f, ok := num(args[len(args)-1]); ok {
					st.fontSize = math.Abs(f)
				}
			}
		case "Tr":
			if len(args) >= 1 {
				if f, ok := num(args[len(args)-1]); ok {
					st.textMode = int64(f)
				}
			}
		case "TL":
			if f, ok := nums(args, 1); ok {
				leading = f[0]
			}
		case "Td", "TD":
			if f, ok := nums(args, 2); ok {
				if op == "TD" {
					leading = -f[1]
				}
				tlm = mat{1, 0, 0, 1, f[0], f[1]}.then(tlm)
				tm = tlm
			}
		case "Tm":
			if f, ok := nums(args, 6); ok {
				tm = mat{f[0], f[1], f[2], f[3], f[4], f[5]}
				tlm = tm
			}
		case "T*":
			tlm = mat{1, 0, 0, 1, 0, -leading}.then(tlm)
			tm = tlm
		case "Tj", "'", `"`:
			if op != "Tj" {
				tlm = mat{1, 0, 0, 1, 0, -leading}.then(tlm)
				tm = tlm
			}
			if len(args) > 0 {
				if s, ok := args[len(args)-1].(pstring); ok {
					show(s)
				}
			}
		case "TJ":
			if len(args) > 0 {
				if a, ok := args[len(args)-1].(array); ok {
					var all []byte
					for _, e := range a {
						if s, ok := e.(pstring); ok {
							all = append(all, s...)
						}
					}
					show(all)
				}
			}
		case "Do":
			if len(args) == 0 {
				return fmt.Errorf("Do without a name")
			}
			n, ok := args[len(args)-1].(name)
			if !ok {
				return fmt.Errorf("Do without a name")
			}
			if err := it.do(string(n), res, st, depth); err != nil {
				return err
			}
		default:
			if !nonPaintingOps[op] {
				return fmt.Errorf("operator %s", op) // BI (inline image) and anything unknown
			}
		}
		args = args[:0]
	}
}

// do handles an XObject: an image is recorded, a form is interpreted in place.
func (it *interp) do(nm string, res dict, st gstate, depth int) error {
	xo, err := it.w.d.derefDict(res["XObject"])
	if err != nil {
		return err
	}
	v, ok := xo[nm]
	r, isRef := v.(ref)
	if !ok || !isRef {
		return fmt.Errorf("XObject %s not found", nm)
	}
	o, gen, so, _, err := it.w.d.resolve(r.num)
	if err != nil {
		return err
	}
	dd, ok := o.(dict)
	if !ok {
		return fmt.Errorf("XObject %s is not a stream", nm)
	}
	switch name2(dd["Subtype"]) {
	case "Image":
		rc := boundsOf(st.ctm, [][2]float64{{0, 0}, {1, 0}, {0, 1}, {1, 1}})
		it.l.order++
		it.l.images = append(it.l.images, imageUse{name: nm, val: v, ctm: st.ctm, r: rc, clip: st.clip, hasClip: st.hasClip, order: it.l.order})
		return nil
	case "Form":
		if depth >= layoutMaxFormDepth || so < 0 {
			return fmt.Errorf("form too deep or not a stream")
		}
		if it.nforms++; it.nforms > 64 {
			return fmt.Errorf("too many forms")
		}
		body, err := it.w.d.streamData(r.num, gen, dd, so)
		if err != nil {
			return err
		}
		fst := st
		m := ident
		if a, ok := dd["Matrix"].(array); ok && len(a) == 6 {
			var f [6]float64
			for i := range f {
				f[i], _ = num(a[i])
			}
			m = mat(f)
		}
		fst.ctm = m.then(st.ctm)
		if bb, ok := it.w.boxRect(dd["BBox"]); ok {
			clip := boundsOf(fst.ctm, [][2]float64{{bb.x0, bb.y0}, {bb.x1, bb.y0}, {bb.x0, bb.y1}, {bb.x1, bb.y1}})
			if fst.hasClip {
				clip = clip.intersect(fst.clip)
			}
			fst.clip, fst.hasClip = clip, true
		}
		fres := res
		if fr, err := it.w.d.derefDict(dd["Resources"]); err == nil {
			fres = fr
		}
		return it.run(body, fres, fst, depth+1)
	}
	return fmt.Errorf("XObject %s is neither image nor form", nm)
}

func (w *walker) boxRect(v any) (rect, bool) {
	x0, y0, x1, y1, ok := w.box(v)
	return rect{x0, y0, x1, y1}, ok
}

// pageRect is the visible page area in user space (CropBox within MediaBox).
func (w *walker) pageRect(inh inherited) rect {
	x0, y0, x1, y1, ok := w.box(inh.media)
	if !ok {
		x0, y0, x1, y1 = 0, 0, 612, 792
	}
	if cx0, cy0, cx1, cy1, ok := w.box(inh.crop); ok {
		nx0, ny0, nx1, ny1 := max(x0, cx0), max(y0, cy0), min(x1, cx1), min(y1, cy1)
		if nx1 > nx0 && ny1 > ny0 {
			x0, y0, x1, y1 = nx0, ny0, nx1, ny1
		} else {
			x0, y0, x1, y1 = cx0, cy0, cx1, cy1
		}
	}
	return rect{x0, y0, x1, y1}
}

// dominantImage decides whether the page is an image-dominant page. On success
// it returns the page's XObject dictionary reduced to that image; otherwise
// the reason, for the log.
func (w *walker) dominantImage(pd dict, inh inherited, res dict) (dict, string) {
	if v, err := w.d.deref(inh.rotate); err == nil {
		if r, ok := v.(int64); ok && r%360 != 0 {
			return nil, "page is rotated"
		}
	}
	content, err := w.pageContent(pd)
	if err != nil || len(content) == 0 {
		return nil, "content unreadable"
	}
	l := &layout{}
	it := &interp{w: w, l: l}
	if err := it.run(content, res, gstate{ctm: ident}, 0); err != nil {
		return nil, "content not understood: " + err.Error()
	}
	if len(l.images) != 1 {
		return nil, fmt.Sprintf("%d images are drawn", len(l.images))
	}
	img := l.images[0]
	m := img.ctm
	if math.Abs(m[1]) > 1e-3*math.Abs(m[0]) || math.Abs(m[2]) > 1e-3*math.Abs(m[3]) || m[0] <= 0 || m[3] <= 0 {
		return nil, "image is rotated or flipped"
	}
	page := w.pageRect(inh)
	if page.area() <= 0 {
		return nil, "empty page box"
	}
	if cover := img.r.intersect(page).area() / page.area(); cover < dominantMinArea {
		return nil, fmt.Sprintf("image covers %.0f%% of the page, needs %.0f%%", cover*100, dominantMinArea*100)
	}
	if img.hasClip && !img.clip.contains(img.r.intersect(page)) {
		return nil, "image is cropped by a clip"
	}
	glyphs, paths := 0, 0
	for _, p := range l.paints {
		switch p.kind {
		case 's':
			return nil, "a shading is drawn"
		case 't':
			glyphs += p.glyphs
		case 'p':
			paths++
		}
		if p.order > img.order && p.r.overlaps(img.r) {
			return nil, "content is drawn over the image"
		}
	}
	if glyphs > maxFurnitureGlyphs {
		return nil, fmt.Sprintf("%d bytes of visible text", glyphs)
	}
	if paths > maxFurniturePaints {
		return nil, fmt.Sprintf("%d vector paints", paths)
	}
	return dict{img.name: img.val}, ""
}
