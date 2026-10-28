// Package cbz synthesises STORED zip archives (CBZ) from image sources
// without materialising them: sizes are known up front, ReadAt works at any
// offset, and CRC32s are computed lazily the first time a header is needed.
package cbz

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
	"sync"
	"time"
)

// ImageSource provides the bytes of image `page`; r is the underlying file.
type ImageSource interface {
	ReadImageAt(r io.ReaderAt, page int, p []byte, off int64) (int, error)
}

// Info becomes a ComicInfo.xml entry when non-nil.
type Info struct {
	Series string
	Number int
}

// ErrTooBig means the part would need ZIP64.
var ErrTooBig = errors.New("cbz: part needs ZIP64")

const (
	sigLocal   = 0x04034b50
	sigCentral = 0x02014b50
	sigEOCD    = 0x06054b50
	maxU32     = 0xFFFFFFFF
	maxEntries = 0xFFFF
)

type entry struct {
	name    string
	inline  []byte
	page    int
	size    int64
	crc     uint32
	hasCRC  bool
	hdrOff  int64
	dataOff int64
}

// Part is one CBZ file.
type Part struct {
	src     ImageSource
	entries []entry
	cdOff   int64
	total   int64
	dosTime uint16
	dosDate uint16

	mu   sync.Mutex
	tail []byte
}

// Img is one image of the archive: its index in the ImageSource, its length
// and the file extension to name it with ("jpg", "png").
type Img struct {
	Page int
	Size int64
	Ext  string
}

// NewPart lays out an archive of images.
func NewPart(src ImageSource, imgs []Img, info *Info, mod time.Time) (*Part, error) {
	p := &Part{src: src}
	p.dosTime, p.dosDate = dosTimeDate(mod)
	width := len(fmt.Sprint(len(imgs)))
	if width < 4 {
		width = 4
	}
	if info != nil {
		x := comicInfo(info)
		p.entries = append(p.entries, entry{name: "ComicInfo.xml", inline: x, size: int64(len(x))})
	}
	for i, im := range imgs {
		ext := im.Ext
		if ext == "" {
			ext = "jpg"
		}
		p.entries = append(p.entries, entry{name: fmt.Sprintf("%0*d.%s", width, i+1, ext), page: im.Page, size: im.Size})
	}
	if len(p.entries) > maxEntries {
		return nil, ErrTooBig
	}
	var off int64
	for i := range p.entries {
		e := &p.entries[i]
		e.hdrOff = off
		e.dataOff = off + 30 + int64(len(e.name))
		off = e.dataOff + e.size
		if e.size > maxU32 {
			return nil, ErrTooBig
		}
	}
	p.cdOff = off
	p.total = off
	for i := range p.entries {
		p.total += 46 + int64(len(p.entries[i].name))
	}
	p.total += 22
	if p.total > maxU32 {
		return nil, ErrTooBig
	}
	return p, nil
}

// Size is the total archive length.
func (p *Part) Size() int64 { return p.total }

func comicInfo(i *Info) []byte {
	return []byte(fmt.Sprintf("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<ComicInfo>\n  <Series>%s</Series>\n  <Number>%d</Number>\n</ComicInfo>\n",
		xmlEscape(i.Series), i.Number))
}

func xmlEscape(s string) string {
	var b []byte
	for _, r := range s {
		switch r {
		case '&':
			b = append(b, "&amp;"...)
		case '<':
			b = append(b, "&lt;"...)
		case '>':
			b = append(b, "&gt;"...)
		case '"':
			b = append(b, "&quot;"...)
		default:
			b = append(b, string(r)...)
		}
	}
	return string(b)
}

func dosTimeDate(t time.Time) (uint16, uint16) {
	t = t.UTC()
	if t.Year() < 1980 {
		t = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return uint16(t.Hour()<<11 | t.Minute()<<5 | t.Second()/2),
		uint16((t.Year()-1980)<<9 | int(t.Month())<<5 | t.Day())
}

func (p *Part) ensureCRC(r io.ReaderAt, i int) error {
	e := &p.entries[i]
	p.mu.Lock()
	ok := e.hasCRC
	p.mu.Unlock()
	if ok {
		return nil
	}
	var sum uint32
	if e.inline != nil {
		sum = crc32.ChecksumIEEE(e.inline)
	} else {
		buf := make([]byte, 256<<10)
		var off int64
		for off < e.size {
			want := int64(len(buf))
			if want > e.size-off {
				want = e.size - off
			}
			n, err := p.src.ReadImageAt(r, e.page, buf[:want], off)
			sum = crc32.Update(sum, crc32.IEEETable, buf[:n])
			off += int64(n)
			if err != nil && int64(n) < want {
				return fmt.Errorf("cbz: reading %s: %w", e.name, err)
			}
		}
	}
	p.mu.Lock()
	e.crc, e.hasCRC = sum, true
	p.mu.Unlock()
	return nil
}

// allCRCs makes sure every entry has its CRC. A source that says reads are
// CPU-bound (rendered pages) lets several run at once; plain file reads stay
// sequential, which is what slow disks want.
func (p *Part) allCRCs(r io.ReaderAt) error {
	par := 1
	if ps, ok := p.src.(interface{ ReadParallelism() int }); ok {
		par = ps.ReadParallelism()
	}
	if par <= 1 {
		for i := range p.entries {
			if err := p.ensureCRC(r, i); err != nil {
				return err
			}
		}
		return nil
	}
	errs := make([]error, len(p.entries))
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i := range p.entries {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			errs[i] = p.ensureCRC(r, i)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Part) header(r io.ReaderAt, i int) ([]byte, error) {
	if err := p.ensureCRC(r, i); err != nil {
		return nil, err
	}
	e := &p.entries[i]
	b := make([]byte, 30, 30+len(e.name))
	le32(b[0:], sigLocal)
	le16(b[4:], 20)
	le16(b[6:], 0x0800)
	le16(b[8:], 0)
	le16(b[10:], p.dosTime)
	le16(b[12:], p.dosDate)
	le32(b[14:], e.crc)
	le32(b[18:], uint32(e.size))
	le32(b[22:], uint32(e.size))
	le16(b[26:], uint16(len(e.name)))
	le16(b[28:], 0)
	return append(b, e.name...), nil
}

func (p *Part) getTail(r io.ReaderAt) ([]byte, error) {
	p.mu.Lock()
	t := p.tail
	p.mu.Unlock()
	if t != nil {
		return t, nil
	}
	if err := p.allCRCs(r); err != nil {
		return nil, err
	}
	b := make([]byte, 0, p.total-p.cdOff)
	for i := range p.entries {
		e := &p.entries[i]
		h := make([]byte, 46)
		le32(h[0:], sigCentral)
		le16(h[4:], 20)
		le16(h[6:], 20)
		le16(h[8:], 0x0800)
		le16(h[10:], 0)
		le16(h[12:], p.dosTime)
		le16(h[14:], p.dosDate)
		le32(h[16:], e.crc)
		le32(h[20:], uint32(e.size))
		le32(h[24:], uint32(e.size))
		le16(h[28:], uint16(len(e.name)))
		le32(h[38:], 0o100644<<16)
		le32(h[42:], uint32(e.hdrOff))
		b = append(append(b, h...), e.name...)
	}
	cdSize := len(b)
	eo := make([]byte, 22)
	le32(eo[0:], sigEOCD)
	le16(eo[8:], uint16(len(p.entries)))
	le16(eo[10:], uint16(len(p.entries)))
	le32(eo[12:], uint32(cdSize))
	le32(eo[16:], uint32(p.cdOff))
	b = append(b, eo...)
	p.mu.Lock()
	p.tail = b
	p.mu.Unlock()
	return b, nil
}

// ReadAt implements io.ReaderAt over the virtual archive; r is the file the
// ImageSource reads from.
func (p *Part) ReadAt(r io.ReaderAt, b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("cbz: negative offset")
	}
	n := 0
	for n < len(b) && off < p.total {
		if off >= p.cdOff {
			t, err := p.getTail(r)
			if err != nil {
				return n, err
			}
			c := copy(b[n:], t[off-p.cdOff:])
			n += c
			off += int64(c)
			continue
		}
		i := sort.Search(len(p.entries), func(k int) bool { return p.entries[k].hdrOff > off }) - 1
		e := &p.entries[i]
		if off < e.dataOff {
			h, err := p.header(r, i)
			if err != nil {
				return n, err
			}
			c := copy(b[n:], h[off-e.hdrOff:])
			n += c
			off += int64(c)
			continue
		}
		rel := off - e.dataOff
		want := int64(len(b) - n)
		if want > e.size-rel {
			want = e.size - rel
		}
		if e.inline != nil {
			c := copy(b[n:n+int(want)], e.inline[rel:])
			n += c
			off += int64(c)
			continue
		}
		m, err := p.src.ReadImageAt(r, e.page, b[n:n+int(want)], rel)
		n += m
		off += int64(m)
		if err != nil && int64(m) < want {
			return n, err
		}
	}
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func le16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }
func le32(b []byte, v uint32) { b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24) }
