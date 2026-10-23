// Package pdfidx locates the per-page JPEG images of simple "one DCT image per
// page" PDFs (scanned books, comics) without loading the file into memory.
// It reads with pread only: xref tables and streams, object streams, the page
// tree, and the standard security handler (RC4, AES-128, AES-256) for
// password-protected files.
package pdfidx

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrUnsupported is wrapped by every error that means "this PDF is not
// convertible"; callers should then expose the file unchanged.
var ErrUnsupported = errors.New("pdfidx: unsupported PDF")

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, a...))
}

const (
	maxStreamBytes = 256 << 20 // refuse to inflate/read bigger structural streams
	maxObjBuf      = 64 << 20
)

type xentry struct {
	typ  byte // 0 free, 1 in file at a, 2 in object stream a at index b
	a, b int64
}

type objStm struct {
	data  []byte
	nums  []int
	offs  []int
	first int
}

type doc struct {
	r    io.ReaderAt
	size int64

	xref    map[int]xentry
	trailer dict
	crypt   *crypt

	mu     sync.Mutex
	objstm map[int]*objStm
}

func (d *doc) readAt(off int64, n int) ([]byte, error) {
	if off < 0 || off > d.size {
		return nil, unsupported("offset %d out of range", off)
	}
	if int64(n) > d.size-off {
		n = int(d.size - off)
	}
	buf := make([]byte, n)
	m, err := d.r.ReadAt(buf, off)
	if m == n {
		return buf, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}

// parseAt parses with growing buffers until the parser stops reporting a
// truncated buffer.
func (d *doc) parseAt(off int64, f func(p *parser) error) error {
	n := 4096
	for {
		buf, err := d.readAt(off, n)
		if err != nil {
			return err
		}
		p := &parser{b: buf}
		err = f(p)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errShort) {
			return unsupported("parse error at %d: %v", off, err)
		}
		if int64(len(buf)) >= d.size-off || n >= maxObjBuf {
			return unsupported("truncated object at %d", off)
		}
		n *= 4
	}
}

// readObjAt parses "N G obj <value> [stream]" at off. streamOff is the file
// offset of the first stream byte, or -1.
func (d *doc) readObjAt(off int64) (num, gen int, o any, streamOff int64, err error) {
	streamOff = -1
	err = d.parseAt(off, func(p *parser) error {
		n, err := p.readInt()
		if err != nil {
			return err
		}
		g, err := p.readInt()
		if err != nil {
			return err
		}
		if err := p.expectKeyword("obj"); err != nil {
			return err
		}
		v, err := p.parseObject()
		if err != nil {
			return err
		}
		num, gen, o = int(n), int(g), v
		if _, ok := v.(dict); ok {
			p.skipWS()
			if p.pos+6 > len(p.b) {
				return errShort
			}
			if bytes.HasPrefix(p.b[p.pos:], []byte("stream")) {
				q := p.pos + 6
				if q >= len(p.b) {
					return errShort
				}
				if p.b[q] == '\r' {
					q++
					if q >= len(p.b) {
						return errShort
					}
				}
				if p.b[q] == '\n' {
					q++
				}
				streamOff = off + int64(q)
			}
		}
		return nil
	})
	return
}

func (d *doc) deref(v any) (any, error) {
	for i := 0; i < 16; i++ {
		r, ok := v.(ref)
		if !ok {
			return v, nil
		}
		o, _, _, _, err := d.resolve(r.num)
		if err != nil {
			return nil, err
		}
		v = o
	}
	return nil, unsupported("reference chain too long")
}

func (d *doc) derefDict(v any) (dict, error) {
	v, err := d.deref(v)
	if err != nil {
		return nil, err
	}
	dc, ok := v.(dict)
	if !ok {
		return nil, unsupported("expected dictionary, got %T", v)
	}
	return dc, nil
}

// resolve returns object num. For in-file stream objects streamOff >= 0.
func (d *doc) resolve(num int) (o any, gen int, streamOff int64, objNum int, err error) {
	e, ok := d.xref[num]
	if !ok || e.typ == 0 {
		return nil, 0, -1, num, nil
	}
	switch e.typ {
	case 1:
		n, g, v, so, err := d.readObjAt(e.a)
		if err != nil {
			return nil, 0, -1, num, err
		}
		if n != num {
			return nil, 0, -1, num, unsupported("xref points to object %d, wanted %d", n, num)
		}
		return v, g, so, num, nil
	case 2:
		s, err := d.loadObjStm(int(e.a))
		if err != nil {
			return nil, 0, -1, num, err
		}
		idx := int(e.b)
		if idx < 0 || idx >= len(s.nums) || s.nums[idx] != num {
			// tolerate wrong index: search
			idx = -1
			for i, n := range s.nums {
				if n == num {
					idx = i
					break
				}
			}
			if idx < 0 {
				return nil, 0, -1, num, unsupported("object %d not in stream %d", num, e.a)
			}
		}
		start := s.first + s.offs[idx]
		if start < 0 || start > len(s.data) {
			return nil, 0, -1, num, unsupported("bad object stream offset")
		}
		p := &parser{b: append(append([]byte{}, s.data[start:]...), ' ')}
		v, err := p.parseObject()
		if err != nil && !errors.Is(err, errShort) {
			return nil, 0, -1, num, unsupported("object stream parse: %v", err)
		}
		if err != nil {
			return nil, 0, -1, num, unsupported("object stream truncated")
		}
		return v, 0, -1, num, nil
	}
	return nil, 0, -1, num, unsupported("bad xref entry type %d", e.typ)
}

func (d *doc) loadObjStm(num int) (*objStm, error) {
	d.mu.Lock()
	s := d.objstm[num]
	d.mu.Unlock()
	if s != nil {
		return s, nil
	}
	o, gen, so, _, err := d.resolve(num)
	if err != nil {
		return nil, err
	}
	dc, ok := o.(dict)
	if !ok || so < 0 {
		return nil, unsupported("object stream %d is not a stream", num)
	}
	data, err := d.streamData(num, gen, dc, so)
	if err != nil {
		return nil, err
	}
	n, _ := dc["N"].(int64)
	first, _ := dc["First"].(int64)
	if n < 0 || n > 1<<22 || first < 0 || first > int64(len(data)) {
		return nil, unsupported("bad object stream header")
	}
	p := &parser{b: append(append([]byte{}, data[:first]...), ' ')}
	s = &objStm{data: data, first: int(first)}
	for i := int64(0); i < n; i++ {
		on, err := p.readInt()
		if err != nil {
			return nil, unsupported("object stream index: %v", err)
		}
		of, err := p.readInt()
		if err != nil {
			return nil, unsupported("object stream index: %v", err)
		}
		s.nums = append(s.nums, int(on))
		s.offs = append(s.offs, int(of))
	}
	d.mu.Lock()
	if len(d.objstm) > 64 {
		d.objstm = map[int]*objStm{}
	}
	d.objstm[num] = s
	d.mu.Unlock()
	return s, nil
}

func (d *doc) streamLength(dc dict) (int64, error) {
	v, err := d.deref(dc["Length"])
	if err != nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok || n < 0 {
		return 0, unsupported("bad stream /Length")
	}
	return n, nil
}

// streamData reads, decrypts and decodes a structural stream (xref, object
// stream). Image streams are never passed through here.
func (d *doc) streamData(num, gen int, dc dict, off int64) ([]byte, error) {
	n, err := d.streamLength(dc)
	if err != nil {
		return nil, err
	}
	if n > maxStreamBytes {
		return nil, unsupported("structural stream too large")
	}
	raw, err := d.readAt(off, int(n))
	if err != nil {
		return nil, err
	}
	if d.crypt != nil && name2(dc["Type"]) != "XRef" {
		raw, err = d.crypt.decryptStream(num, gen, raw)
		if err != nil {
			return nil, err
		}
	}
	return d.decode(dc, raw)
}

func name2(v any) string {
	n, _ := v.(name)
	return string(n)
}

func (d *doc) filters(dc dict) ([]string, []dict, error) {
	f, err := d.deref(dc["Filter"])
	if err != nil {
		return nil, nil, err
	}
	pv, err := d.deref(dc["DecodeParms"])
	if err != nil {
		return nil, nil, err
	}
	var names []string
	var parms []dict
	switch t := f.(type) {
	case nil:
	case name:
		names = []string{string(t)}
		pd, _ := pv.(dict)
		parms = []dict{pd}
	case array:
		pa, _ := pv.(array)
		for i, e := range t {
			e, err := d.deref(e)
			if err != nil {
				return nil, nil, err
			}
			n, ok := e.(name)
			if !ok {
				return nil, nil, unsupported("bad filter entry")
			}
			names = append(names, string(n))
			var pd dict
			if i < len(pa) {
				pd, _ = pa[i].(dict)
			}
			parms = append(parms, pd)
		}
	default:
		return nil, nil, unsupported("bad /Filter")
	}
	return names, parms, nil
}

func (d *doc) decode(dc dict, raw []byte) ([]byte, error) {
	names, parms, err := d.filters(dc)
	if err != nil {
		return nil, err
	}
	for i, n := range names {
		switch n {
		case "FlateDecode", "Fl":
			zr, err := zlib.NewReader(bytes.NewReader(raw))
			if err != nil {
				return nil, unsupported("flate: %v", err)
			}
			out, err := io.ReadAll(io.LimitReader(zr, maxStreamBytes+1))
			if err != nil && len(out) == 0 {
				return nil, unsupported("flate: %v", err)
			}
			if len(out) > maxStreamBytes {
				return nil, unsupported("inflated stream too large")
			}
			raw = out
			if pd := parms[i]; pd != nil {
				raw, err = applyPredictor(pd, raw)
				if err != nil {
					return nil, err
				}
			}
		default:
			return nil, unsupported("stream filter %s", n)
		}
	}
	return raw, nil
}

func intOf(d dict, k string, def int64) int64 {
	if v, ok := d[k].(int64); ok {
		return v
	}
	return def
}

func applyPredictor(pd dict, in []byte) ([]byte, error) {
	pred := intOf(pd, "Predictor", 1)
	if pred <= 1 {
		return in, nil
	}
	if pred < 10 {
		return nil, unsupported("TIFF predictor")
	}
	colors := intOf(pd, "Colors", 1)
	bpc := intOf(pd, "BitsPerComponent", 8)
	cols := intOf(pd, "Columns", 1)
	rowBytes := int((colors*bpc*cols + 7) / 8)
	bpp := int((colors*bpc + 7) / 8)
	if rowBytes <= 0 || bpp <= 0 {
		return nil, unsupported("bad predictor params")
	}
	stride := rowBytes + 1
	rows := len(in) / stride
	out := make([]byte, rows*rowBytes)
	prev := make([]byte, rowBytes)
	for r := 0; r < rows; r++ {
		ft := in[r*stride]
		src := in[r*stride+1 : (r+1)*stride]
		cur := out[r*rowBytes : (r+1)*rowBytes]
		switch ft {
		case 0:
			copy(cur, src)
		case 1:
			for i := range cur {
				var l byte
				if i >= bpp {
					l = cur[i-bpp]
				}
				cur[i] = src[i] + l
			}
		case 2:
			for i := range cur {
				cur[i] = src[i] + prev[i]
			}
		case 3:
			for i := range cur {
				var l byte
				if i >= bpp {
					l = cur[i-bpp]
				}
				cur[i] = src[i] + byte((int(l)+int(prev[i]))/2)
			}
		case 4:
			for i := range cur {
				var a, c byte
				if i >= bpp {
					a = cur[i-bpp]
					c = prev[i-bpp]
				}
				cur[i] = src[i] + paeth(a, prev[i], c)
			}
		default:
			return nil, unsupported("bad PNG predictor row type %d", ft)
		}
		prev = cur
	}
	return out, nil
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
