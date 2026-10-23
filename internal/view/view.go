// Package view decides how PDFs are presented: which are convertible, how
// they are split into CBZ parts, what the virtual entries are called, and
// their stable inode numbers.
package view

import (
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tonyupup/pdfcbzfs/internal/cbz"
	"github.com/tonyupup/pdfcbzfs/internal/pdfidx"
)

type Mode string

const (
	Auto   Mode = "auto"   // <= PagesPerCBZ pages: a single .cbz in place; more: a directory of parts
	Folder Mode = "folder" // always a directory of parts
)

type Config struct {
	PagesPerCBZ int
	Mode        Mode
	Logf        func(format string, a ...any)
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
	Folder bool
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

func (p *PDF) SingleName() string { return p.Stem + ".cbz" }
func (p *PDF) DirName() string    { return p.Stem }

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
	return &Cache{cfg: cfg, m: map[string]*entry{}}
}

// Probe returns the PDF view, or nil if the file must be passed through.
func (c *Cache) Probe(path string, st *syscall.Stat_t) *PDF {
	pwSig := "-"
	if ps, err := os.Stat(path + ".passwd"); err == nil {
		pwSig = fmt.Sprintf("%d:%d", ps.Size(), ps.ModTime().UnixNano())
	}
	sig := fmt.Sprintf("%d:%d:%d:%s", st.Size, st.Mtim.Nano(), st.Ino, pwSig)
	c.mu.Lock()
	e := c.m[path]
	if e == nil || e.sig != sig {
		e = &entry{sig: sig}
		c.m[path] = e
	}
	c.mu.Unlock()
	e.once.Do(func() {
		p, err := c.build(path, st)
		if err != nil {
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

func (c *Cache) build(path string, st *syscall.Stat_t) (*PDF, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	idx, err := pdfidx.Open(f, st.Size, readPasswords(path)...)
	if err != nil {
		return nil, err
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
	p.Folder = c.cfg.Mode == Folder || n > per
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
		if p.Folder {
			info = &cbz.Info{Series: stem, Number: part}
		}
		cp, err := cbz.NewPart(idx, imgs, info, p.Mod)
		if err != nil {
			return nil, err
		}
		p.Parts = append(p.Parts, cp)
		if !p.Folder {
			break
		}
	}
	return p, nil
}
