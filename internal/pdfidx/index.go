package pdfidx

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Page describes the single JPEG image of one PDF page.
type Page struct {
	Off  int64 // offset of the (possibly encrypted) stream data in the file
	Len  int64 // stored length
	Size int64 // length of the JPEG after decryption
	Num  int   // object number / generation, for the per-object key
	Gen  int
	Ext  string // "jpg" (embedded JPEG) or "png" (synthesised from raw samples)

	png *pngSpec
}

// Index is the result of scanning a PDF. It is safe for concurrent use.
type Index struct {
	Pages []Page

	crypt *crypt
}

// Encrypted reports whether image reads go through a decryption step.
// (PNG pages are synthesised for every document, encrypted or not.)
func (x *Index) Encrypted() bool { return x.crypt != nil && !x.crypt.identity() }

// Open scans the PDF. passwords are tried (as user, then owner password) after
// the empty password. It never reads more than the structural objects.
func Open(r io.ReaderAt, size int64, passwords ...[]byte) (x *Index, err error) {
	defer func() {
		if e := recover(); e != nil {
			x, err = nil, unsupported("parser panic: %v", e)
		}
	}()
	if size < 32 {
		return nil, unsupported("file too small")
	}
	// PDF offsets count from the "%PDF-" header, which may follow junk bytes.
	base := headerOffset(r, size)
	if base > 0 {
		r, size = io.NewSectionReader(r, base, size-base), size-base
	}
	d := &doc{r: r, size: size, base: base, objstm: map[int]*objStm{}}
	if err := d.loadXref(); err != nil {
		return nil, err
	}
	if err := d.setupCrypt(passwords); err != nil {
		return nil, err
	}
	rootV, err := d.deref(d.trailer["Root"])
	if err != nil {
		return nil, err
	}
	root, ok := rootV.(dict)
	if !ok {
		return nil, unsupported("no catalog")
	}
	x = &Index{crypt: d.crypt}
	w := &walker{d: d, seen: map[int]bool{}}
	x.Pages, err = w.node(root["Pages"], nil, 0)
	if err != nil {
		return nil, err
	}
	if len(x.Pages) == 0 {
		return nil, unsupported("no pages")
	}
	return x, nil
}

type walker struct {
	d *doc

	mu   sync.Mutex
	seen map[int]bool
}

// walkers bounds the parallel preads while resolving a page tree: pages are
// scattered through the file between their images, so on a cold disk or a
// network filesystem the round trips dominate.
const walkers = 16

func (w *walker) node(v any, res any, depth int) ([]Page, error) {
	if depth > 64 {
		return nil, unsupported("page tree too deep")
	}
	if r, ok := v.(ref); ok {
		w.mu.Lock()
		dup := w.seen[r.num]
		w.seen[r.num] = true
		w.mu.Unlock()
		if dup {
			return nil, unsupported("page tree cycle")
		}
	}
	dc, err := w.d.derefDict(v)
	if err != nil {
		return nil, err
	}
	if r, ok := dc["Resources"]; ok {
		res = r
	}
	if kids, ok := dc["Kids"]; ok && name2(dc["Type"]) != "Page" {
		ka, err := w.d.deref(kids)
		if err != nil {
			return nil, err
		}
		arr, ok := ka.(array)
		if !ok {
			return nil, unsupported("bad /Kids")
		}
		return w.kids(arr, res, depth)
	}
	pg, err := w.page(dc, res)
	if err != nil {
		return nil, err
	}
	return []Page{pg}, nil
}

func (w *walker) kids(arr array, res any, depth int) ([]Page, error) {
	results := make([][]Page, len(arr))
	errs := make([]error, len(arr))
	n := walkers
	if len(arr) < n {
		n = len(arr)
	}
	idx := make(chan int)
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				func() {
					defer func() {
						if e := recover(); e != nil {
							errs[i] = unsupported("parser panic: %v", e)
						}
					}()
					results[i], errs[i] = w.node(arr[i], res, depth+1)
				}()
			}
		}()
	}
	for i := range arr {
		idx <- i
	}
	close(idx)
	wg.Wait()
	var out []Page
	for i := range arr {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out = append(out, results[i]...)
	}
	return out, nil
}

func (w *walker) page(pd dict, res any) (Page, error) {
	var zero Page
	rd, err := w.d.derefDict(res)
	if err != nil {
		return zero, unsupported("page: no resources")
	}
	xo, err := w.d.derefDict(rd["XObject"])
	if err != nil || len(xo) != 1 {
		return zero, unsupported("page needs exactly one XObject (%s)", w.describeXObjects(xo))
	}
	var iref ref
	for _, v := range xo {
		r, ok := v.(ref)
		if !ok {
			return zero, unsupported("page has a direct XObject")
		}
		iref = r
	}
	o, gen, so, _, err := w.d.resolve(iref.num)
	if err != nil {
		return zero, err
	}
	id, ok := o.(dict)
	if !ok || so < 0 {
		return zero, unsupported("page image is not a stream")
	}
	if name2(id["Subtype"]) != "Image" {
		return zero, unsupported("page XObject is not an image")
	}
	names, parms, err := w.d.filters(id)
	if err != nil {
		return zero, err
	}
	if len(names) != 1 {
		return zero, unsupported("image filters %v", names)
	}
	mask := id["ImageMask"] == true
	invert := false
	if dec, ok := id["Decode"]; ok {
		a, _ := dec.(array)
		if !mask || len(a) != 2 {
			return zero, unsupported("image has /Decode")
		}
		switch {
		case numIs(a[0], 0) && numIs(a[1], 1):
		case numIs(a[0], 1) && numIs(a[1], 0):
			invert = true
		default:
			return zero, unsupported("image has /Decode")
		}
	}
	if !mask {
		for _, k := range []string{"SMask", "Mask"} {
			if v, ok := id[k]; ok && v != false {
				return zero, unsupported("image has /%s", k)
			}
		}
	} else if _, ok := id["SMask"]; ok {
		return zero, unsupported("image mask has /SMask")
	}
	l, err := w.d.streamLength(id)
	if err != nil {
		return zero, err
	}
	if so+l > w.d.size {
		return zero, unsupported("image stream past end of file")
	}
	switch names[0] {
	case "DCTDecode", "DCT":
		if cs, ok := id["ColorSpace"].(name); ok && cs == "DeviceCMYK" {
			return zero, unsupported("CMYK JPEG")
		}
		size := l
		if w.d.crypt != nil {
			size, err = w.d.crypt.plainSize(w.d.r, iref.num, gen, so, l)
			if err != nil {
				return zero, err
			}
		}
		return Page{Off: so + w.d.base, Len: l, Size: size, Num: iref.num, Gen: gen, Ext: "jpg"}, nil
	case "FlateDecode", "Fl":
		spec, err := w.d.pngSpecFor(id, iref.num, gen, parms[0], mask, invert)
		if err != nil {
			return zero, unsupported("%v", err)
		}
		return Page{Off: so + w.d.base, Len: l, Size: spec.pngSize(), Num: iref.num, Gen: gen, Ext: "png", png: spec}, nil
	case "CCITTFaxDecode", "CCF":
		cc, err := ccittOf(parms[0])
		if err != nil {
			return zero, err
		}
		spec, err := w.d.pngSpecFor(id, iref.num, gen, parms[0], mask, invert)
		if err != nil {
			return zero, unsupported("%v", err)
		}
		if spec.bpc != 1 || spec.colors != 1 {
			return zero, unsupported("CCITT image is not 1-bit")
		}
		if cols := intOf(parms[0], "Columns", 1728); cols != int64(spec.w) {
			return zero, unsupported("CCITT /Columns %d != /Width %d", cols, spec.w)
		}
		spec.ccitt = cc
		return Page{Off: so + w.d.base, Len: l, Size: spec.pngSize(), Num: iref.num, Gen: gen, Ext: "png", png: spec}, nil
	}
	if mask {
		return zero, unsupported("image mask with filter %s", names[0])
	}
	return zero, unsupported("image filter %s", names[0])
}

// ReadImageAt reads the JPEG of page at offset off, like io.ReaderAt.
func (x *Index) ReadImageAt(r io.ReaderAt, page int, p []byte, off int64) (int, error) {
	pg := x.Pages[page]
	if off >= pg.Size {
		return 0, io.EOF
	}
	if !x.Encrypted() && pg.png == nil {
		want := int64(len(p))
		short := false
		if want > pg.Size-off {
			want, short = pg.Size-off, true
		}
		n, err := r.ReadAt(p[:want], pg.Off+off)
		if err == nil && short {
			err = io.EOF
		}
		return n, err
	}
	img, err := x.image(r, page)
	if err != nil {
		return 0, err
	}
	n := copy(p, img[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (x *Index) image(r io.ReaderAt, page int) ([]byte, error) {
	if b, ok := imgs.get(x, page); ok {
		return b, nil
	}
	pg := x.Pages[page]
	if pg.Len > maxStreamBytes {
		return nil, fmt.Errorf("pdfidx: image too large")
	}
	raw := make([]byte, pg.Len)
	if _, err := r.ReadAt(raw, pg.Off); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var err error
	if x.crypt != nil {
		if raw, err = x.crypt.decryptStream(pg.Num, pg.Gen, raw); err != nil {
			return nil, err
		}
	}
	if pg.png != nil {
		rows, err := pg.png.decodeRaw(raw)
		if err != nil {
			return nil, err
		}
		raw, err = pg.png.buildPNG(rows)
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) != pg.Size {
			return nil, fmt.Errorf("pdfidx: PNG size %d, expected %d", len(raw), pg.Size)
		}
	}
	imgs.put(x, page, raw)
	return raw, nil
}

// imgs is one byte-bounded LRU shared by every Index, so memory stays flat
// however many PDFs a library holds (a per-Index cache grew with PDF count).
var imgs = newImgCache(32 << 20)

// imgCache is a byte-bounded LRU of decrypted images.
type imgCache struct {
	mu    sync.Mutex
	max   int
	used  int
	ll    *list.List
	items map[cacheKey]*list.Element
}

type cacheKey struct {
	x    *Index
	page int
}

type cacheItem struct {
	key cacheKey
	b   []byte
}

func newImgCache(max int) *imgCache {
	return &imgCache{max: max, ll: list.New(), items: map[cacheKey]*list.Element{}}
}

func (c *imgCache) get(x *Index, page int) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[cacheKey{x, page}]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*cacheItem).b, true
	}
	return nil, false
}

func (c *imgCache) put(x *Index, page int, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := cacheKey{x, page}
	if _, ok := c.items[k]; ok {
		return
	}
	c.items[k] = c.ll.PushFront(&cacheItem{k, b})
	c.used += len(b)
	for c.used > c.max && c.ll.Len() > 1 {
		e := c.ll.Back()
		it := e.Value.(*cacheItem)
		c.ll.Remove(e)
		delete(c.items, it.key)
		c.used -= len(it.b)
	}
}

// numIs reports whether a parsed PDF number equals want (ints or reals).
func numIs(v any, want int64) bool {
	switch n := v.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// ccittOf reads /DecodeParms of a CCITTFaxDecode image. Only Group 4 (K < 0)
// is supported: Group 3 streams routinely lack the EOL codes the decoder needs.
func ccittOf(parms dict) (*ccittSpec, error) {
	k, _ := parms["K"].(int64)
	if _, isReal := parms["K"].(float64); isReal || k >= 0 {
		return nil, unsupported("CCITT Group 3 (K=%v)", parms["K"])
	}
	c := &ccittSpec{}
	c.align, _ = parms["EncodedByteAlign"].(bool)
	c.blackIs1, _ = parms["BlackIs1"].(bool)
	return c, nil
}

// headerOffset returns the position of "%PDF-" within the first KiB (the
// range readers accept), or 0 when there is none.
func headerOffset(r io.ReaderAt, size int64) int64 {
	n := int64(1024)
	if n > size {
		n = size
	}
	b := make([]byte, n)
	m, _ := r.ReadAt(b, 0)
	if i := bytes.Index(b[:m], []byte("%PDF-")); i > 0 {
		return int64(i)
	}
	return 0
}

// describeXObjects summarises a page's XObjects for the "unsupported" log.
func (w *walker) describeXObjects(xo dict) string {
	if len(xo) == 0 {
		return "none: text or vector page, nothing to extract"
	}
	kinds := map[string]int{}
	for _, v := range xo {
		kind := "?"
		if r, ok := v.(ref); ok {
			if o, _, _, _, err := w.d.resolve(r.num); err == nil {
				if dd, ok := o.(dict); ok {
					kind = string(name2(dd["Subtype"]))
				}
			}
		}
		kinds[kind]++
	}
	out := fmt.Sprintf("found %d:", len(xo))
	for _, k := range []string{"Image", "Form", "?"} {
		if kinds[k] > 0 {
			out += fmt.Sprintf(" %s=%d", k, kinds[k])
		}
	}
	return out
}
