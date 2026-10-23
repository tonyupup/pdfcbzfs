package pdfidx

import (
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
}

// Index is the result of scanning a PDF. It is safe for concurrent use.
type Index struct {
	Pages []Page

	crypt *crypt
	cache *imgCache
}

// Encrypted reports whether image reads go through a decryption step.
func (x *Index) Encrypted() bool { return x.crypt != nil && !x.crypt.identity() }

// Open scans the PDF. passwords are tried (as user, then owner password) after
// the empty password. It never reads more than the structural objects.
func Open(r io.ReaderAt, size int64, passwords ...[]byte) (x *Index, err error) {
	defer func() {
		if e := recover(); e != nil {
			x, err = nil, unsupported("parser panic: %v", e)
		}
	}()
	d := &doc{r: r, size: size, objstm: map[int]*objStm{}}
	if size < 32 {
		return nil, unsupported("file too small")
	}
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
	x = &Index{crypt: d.crypt, cache: newImgCache(64 << 20)}
	w := &walker{d: d, x: x, seen: map[int]bool{}}
	if err := w.node(root["Pages"], nil, 0); err != nil {
		return nil, err
	}
	if len(x.Pages) == 0 {
		return nil, unsupported("no pages")
	}
	return x, nil
}

type walker struct {
	d    *doc
	x    *Index
	seen map[int]bool
}

func (w *walker) node(v any, res any, depth int) error {
	if depth > 64 {
		return unsupported("page tree too deep")
	}
	if r, ok := v.(ref); ok {
		if w.seen[r.num] {
			return unsupported("page tree cycle")
		}
		w.seen[r.num] = true
	}
	dc, err := w.d.derefDict(v)
	if err != nil {
		return err
	}
	if r, ok := dc["Resources"]; ok {
		res = r
	}
	if kids, ok := dc["Kids"]; ok && name2(dc["Type"]) != "Page" {
		ka, err := w.d.deref(kids)
		if err != nil {
			return err
		}
		arr, ok := ka.(array)
		if !ok {
			return unsupported("bad /Kids")
		}
		for _, k := range arr {
			if err := w.node(k, res, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return w.page(dc, res)
}

func (w *walker) page(pd dict, res any) error {
	n := len(w.x.Pages) + 1
	rd, err := w.d.derefDict(res)
	if err != nil {
		return unsupported("page %d: no resources", n)
	}
	xo, err := w.d.derefDict(rd["XObject"])
	if err != nil || len(xo) != 1 {
		return unsupported("page %d: needs exactly one XObject", n)
	}
	var iref ref
	for _, v := range xo {
		r, ok := v.(ref)
		if !ok {
			return unsupported("page %d: direct XObject", n)
		}
		iref = r
	}
	o, gen, so, _, err := w.d.resolve(iref.num)
	if err != nil {
		return err
	}
	id, ok := o.(dict)
	if !ok || so < 0 {
		return unsupported("page %d: image is not a stream", n)
	}
	if name2(id["Subtype"]) != "Image" {
		return unsupported("page %d: XObject is not an image", n)
	}
	names, _, err := w.d.filters(id)
	if err != nil {
		return err
	}
	if len(names) != 1 || (names[0] != "DCTDecode" && names[0] != "DCT") {
		return unsupported("page %d: image filter %v is not DCTDecode", n, names)
	}
	if _, ok := id["Decode"]; ok {
		return unsupported("page %d: image has /Decode", n)
	}
	if cs, ok := id["ColorSpace"].(name); ok && cs == "DeviceCMYK" {
		return unsupported("page %d: CMYK JPEG", n)
	}
	l, err := w.d.streamLength(id)
	if err != nil {
		return err
	}
	if so+l > w.d.size {
		return unsupported("page %d: stream past end of file", n)
	}
	size := l
	if w.d.crypt != nil {
		size, err = w.d.crypt.plainSize(w.d.r, iref.num, gen, so, l)
		if err != nil {
			return err
		}
	}
	w.x.Pages = append(w.x.Pages, Page{Off: so, Len: l, Size: size, Num: iref.num, Gen: gen})
	return nil
}

// ReadImageAt reads the JPEG of page at offset off, like io.ReaderAt.
func (x *Index) ReadImageAt(r io.ReaderAt, page int, p []byte, off int64) (int, error) {
	pg := x.Pages[page]
	if off >= pg.Size {
		return 0, io.EOF
	}
	if !x.Encrypted() {
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
	if b, ok := x.cache.get(page); ok {
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
	b, err := x.crypt.decryptStream(pg.Num, pg.Gen, raw)
	if err != nil {
		return nil, err
	}
	x.cache.put(page, b)
	return b, nil
}

// imgCache is a byte-bounded LRU of decrypted images.
type imgCache struct {
	mu    sync.Mutex
	max   int
	used  int
	ll    *list.List
	items map[int]*list.Element
}

type cacheItem struct {
	page int
	b    []byte
}

func newImgCache(max int) *imgCache {
	return &imgCache{max: max, ll: list.New(), items: map[int]*list.Element{}}
}

func (c *imgCache) get(page int) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[page]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*cacheItem).b, true
	}
	return nil, false
}

func (c *imgCache) put(page int, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[page]; ok {
		return
	}
	c.items[page] = c.ll.PushFront(&cacheItem{page, b})
	c.used += len(b)
	for c.used > c.max && c.ll.Len() > 1 {
		e := c.ll.Back()
		it := e.Value.(*cacheItem)
		c.ll.Remove(e)
		delete(c.items, it.page)
		c.used -= len(it.b)
	}
}
