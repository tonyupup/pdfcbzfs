// Package view decides how PDFs are presented: which are convertible, how
// they are split into CBZ parts, what the virtual entries are called, and
// their stable inode numbers.
package view

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tonyupup/pdfcbzfs/internal/cbz"
	"github.com/tonyupup/pdfcbzfs/internal/pdfidx"
	"github.com/tonyupup/pdfcbzfs/internal/render"
)

type Mode string

const (
	Auto   Mode = "auto"   // <= PagesPerCBZ pages: a single .cbz in place; more: a directory of parts
	Folder Mode = "folder" // always a directory of parts
	Flat   Mode = "flat"   // <= PagesPerCBZ pages: a single .cbz; more: "Name vNN.cbz" parts next to where the PDF was
)

// Renderer draws pages that cannot be extracted from their PDF. *render.Service
// implements it.
type Renderer interface {
	Available() bool
	Page(ctx context.Context, k render.PageKey) ([]byte, error)
}

type Config struct {
	PagesPerCBZ int
	Mode        Mode
	Logf        func(format string, a ...any)
	// Render, when set and available, lets PDFs with pages that cannot be
	// extracted (text, several images, JPEG 2000) be converted by drawing
	// those pages. Without it such PDFs are passed through.
	Render Renderer
	// Now is the clock (tests).
	Now func() time.Time
}

// PDF is a convertible PDF and its virtual presentation.
type PDF struct {
	Path   string
	Stem   string
	Size   int64
	Mod    time.Time
	Ino    uint64
	Dev    uint64
	Uid    uint32
	Gid    uint32
	Idx    *pdfidx.Index
	Parts  []*cbz.Part
	Folder bool // parts live in a directory named after the PDF
	Flat   bool // several parts, placed directly in the PDF's directory
}

// IsPDFName reports whether name has a .pdf extension (any case) and returns
// the stem.
func IsPDFName(name string) (stem string, ok bool) {
	if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".pdf") {
		return name[:len(name)-4], true
	}
	return "", false
}

// IsCBZName reports whether name has a .cbz extension and returns the stem.
func IsCBZName(name string) (stem string, ok bool) {
	if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".cbz") {
		return name[:len(name)-4], true
	}
	return "", false
}

// Single reports whether the PDF is presented as exactly one Stem.cbz.
func (p *PDF) Single() bool { return !p.Folder && !p.Flat }

func (p *PDF) SingleName() string { return p.Stem + ".cbz" }
func (p *PDF) DirName() string    { return p.Stem }

// FlatName returns the file name of part i for a Flat PDF.
func (p *PDF) FlatName(i int) string { return p.PartName(i) }

// SplitPartName splits "Stem vNN" (the stem of a part's file name) into the
// PDF stem and the 1-based part number.
func SplitPartName(stem string) (base string, part int, ok bool) {
	i := strings.LastIndex(stem, " v")
	if i <= 0 || i+2 >= len(stem) {
		return "", 0, false
	}
	n, err := strconv.Atoi(stem[i+2:])
	if err != nil || n < 1 || stem[i+2] == '+' || stem[i+2] == '-' {
		return "", 0, false
	}
	return stem[:i], n, true
}

func (p *PDF) PartName(i int) string {
	w := len(strconv.Itoa(len(p.Parts)))
	if w < 2 {
		w = 2
	}
	return fmt.Sprintf("%s v%0*d.cbz", p.Stem, w, i+1)
}

// Virtual inode numbers have the top bit set so they never collide with the
// lower filesystem's inodes (used verbatim as node IDs, which NFS needs).
func (p *PDF) vino(kind byte, part int) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d/%d/%c/%d", p.Dev, p.Ino, kind, part)
	return h.Sum64() | 1<<63
}

func (p *PDF) DirIno() uint64       { return p.vino('d', 0) }
func (p *PDF) PartIno(i int) uint64 { return p.vino('f', i) }
func (p *PDF) SingleIno() uint64    { return p.vino('f', 0) }

// Gen changes whenever the PDF does, so a reused inode number is never
// mistaken for the old file (an NFS requirement).
func (p *PDF) Gen() uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d/%d", p.Size, p.Mod.UnixNano())
	return h.Sum64() | 1
}

// Cache probes PDFs and remembers the result (including "not convertible")
// until the file or its .passwd changes.
type Cache struct {
	cfg Config
	mu  sync.Mutex
	m   map[string]*entry
}

type entry struct {
	sig  string
	once sync.Once
	pdf  *PDF

	// A failure that may go away (the renderer broke, a page would not draw)
	// is probed again after a backoff instead of being remembered for good.
	fails   int
	retryAt atomic.Int64 // unix nanoseconds; 0: final
}

// transient marks a probe failure worth retrying later.
type transient struct{ err error }

func (t *transient) Error() string { return t.err.Error() }
func (t *transient) Unwrap() error { return t.err }

// probeBackoff is the wait before probing a PDF again after its n-th
// transient failure.
func probeBackoff(n int) time.Duration {
	d := 30 * time.Second << min(max(n-1, 0), 5) // 30s, 1m, 2m, 4m, 8m, 16m
	return min(d, 15*time.Minute)
}

func NewCache(cfg Config) *Cache {
	if cfg.PagesPerCBZ <= 0 {
		cfg.PagesPerCBZ = 200
	}
	if cfg.Mode == "" {
		cfg.Mode = Auto
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Cache{cfg: cfg, m: map[string]*entry{}}
}

// Probe returns the PDF view, or nil if the file must be passed through.
func (c *Cache) Probe(path string, st *syscall.Stat_t) *PDF {
	pwSig := "-"
	if ps, err := os.Stat(path + ".passwd"); err == nil {
		pwSig = fmt.Sprintf("%d:%d", ps.Size(), ps.ModTime().UnixNano())
	}
	// aria2 keeps NAME.aria2 beside a file it is still downloading (the file
	// itself is preallocated, so it looks complete but is not).
	_, aerr := os.Stat(path + ".aria2")
	partial := aerr == nil
	rendering := c.cfg.Render != nil && c.cfg.Render.Available()
	sig := fmt.Sprintf("%d:%d:%d:%s:%t:%t", st.Size, st.Mtim.Nano(), st.Ino, pwSig, partial, rendering)
	c.mu.Lock()
	e := c.m[path]
	switch {
	case e == nil || e.sig != sig:
		e = &entry{sig: sig}
		c.m[path] = e
	case e.retryAt.Load() != 0 && c.cfg.Now().UnixNano() >= e.retryAt.Load():
		// retryAt is set (after fails) only by a failed probe, so reading it
		// is safe while another goroutine may still be inside e.once.Do
		e = &entry{sig: sig, fails: e.fails} // the backoff is over: probe again
		c.m[path] = e
	}
	c.mu.Unlock()
	e.once.Do(func() {
		if partial {
			c.cfg.Logf("pdf passthrough %s: download in progress (%s.aria2 exists)", path, path)
			return
		}
		p, err := c.build(path, st, rendering)
		if err != nil {
			var tr *transient
			if errors.As(err, &tr) {
				e.fails++
				wait := probeBackoff(e.fails)
				e.retryAt.Store(c.cfg.Now().Add(wait).UnixNano())
				c.cfg.Logf("pdf passthrough %s: %v (trying again in %v)", path, err, wait)
				return
			}
			c.cfg.Logf("pdf passthrough %s: %v", path, err)
			return
		}
		e.pdf = p
	})
	return e.pdf
}

func readPasswords(path string) [][]byte {
	b, err := os.ReadFile(path + ".passwd")
	if err != nil || len(b) > 4096 {
		return nil
	}
	var out [][]byte
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" {
			continue
		}
		out = append(out, []byte(l))
		if t := strings.TrimSpace(l); t != l && t != "" {
			out = append(out, []byte(t))
		}
	}
	return out
}

func (c *Cache) build(path string, st *syscall.Stat_t, rendering bool) (*PDF, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	idx, err := pdfidx.OpenWith(f, st.Size, pdfidx.Options{Render: rendering}, readPasswords(path)...)
	if err != nil {
		return nil, err
	}
	if n, first := idx.RenderPages(); n > 0 {
		if err := c.attachRenderer(idx, path, st, n, first); err != nil {
			return nil, err
		}
	}
	if len(idx.Skipped) > 0 {
		c.cfg.Logf("pdf %s: skipped %d missing page object(s) %v", path, len(idx.Skipped), idx.Skipped)
	}
	name := path[strings.LastIndexByte(path, '/')+1:]
	stem, _ := IsPDFName(name)
	p := &PDF{
		Path: path, Stem: stem, Size: st.Size,
		Mod: time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)),
		Ino: st.Ino, Dev: uint64(st.Dev), Uid: st.Uid, Gid: st.Gid,
		Idx: idx,
	}
	n := len(idx.Pages)
	per := c.cfg.PagesPerCBZ
	p.Folder = c.cfg.Mode == Folder || (c.cfg.Mode == Auto && n > per)
	p.Flat = c.cfg.Mode == Flat && n > per
	multi := p.Folder || p.Flat
	for first, part := 0, 1; first < n; first, part = first+per, part+1 {
		last := first + per
		if last > n {
			last = n
		}
		imgs := make([]cbz.Img, 0, last-first)
		for i := first; i < last; i++ {
			imgs = append(imgs, cbz.Img{Page: i, Size: idx.Pages[i].Size, Ext: idx.Pages[i].Ext})
		}
		var info *cbz.Info
		if multi {
			info = &cbz.Info{Series: stem, Number: part}
		}
		cp, err := cbz.NewPart(idx, imgs, info, p.Mod)
		if err != nil {
			return nil, err
		}
		p.Parts = append(p.Parts, cp)
		if !multi {
			break
		}
	}
	return p, nil
}

// attachRenderer connects the index's render pages to the renderer and draws
// the first one right away. That proves the helper can open this file (right
// password, not damaged beyond repair) before the PDF is hidden behind a CBZ;
// the result is cached, so the page is not drawn twice.
func (c *Cache) attachRenderer(idx *pdfidx.Index, path string, st *syscall.Stat_t, n, first int) error {
	id := fmt.Sprintf("%d:%d:%d:%d", st.Dev, st.Ino, st.Size, st.Mtim.Nano())
	idx.SetRenderer(func(i int) ([]byte, error) {
		pg := idx.Pages[i]
		return c.cfg.Render.Page(context.Background(), render.PageKey{
			FileID: id, Path: path, Password: idx.Password(), Page: pg.Render.Index,
			Budget: pg.Size, DPI: pdfidx.RenderDPI, MaxSide: pdfidx.RenderMaxSide,
		})
	})
	if _, err := idx.RenderedPage(first); err != nil {
		return &transient{fmt.Errorf("renderer cannot draw page %d: %w", first+1, err)}
	}
	c.cfg.Logf("pdf %s: %d of %d pages are drawn by the renderer (page %d: %s)",
		path, n, len(idx.Pages), first+1, idx.Pages[first].Render.Reason)
	return nil
}
