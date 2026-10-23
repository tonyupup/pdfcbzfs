// Package fusefs is a loopback (passthrough) FUSE filesystem in which every
// convertible PDF is shown as CBZ file(s) instead.
package fusefs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/tonyupup/pdfcbzfs/internal/view"
)

// FS holds the state shared by all nodes.
type FS struct {
	Lower string
	Root  fs.InodeEmbedder

	loop  *fs.LoopbackRoot
	cache *view.Cache

	idxMu    sync.Mutex
	idx      map[uint64][]string
	idxBuilt time.Time
}

// New builds the filesystem over the real directory lower.
func New(lower string, cfg view.Config) (*FS, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(lower, &st); err != nil {
		return nil, err
	}
	f := &FS{Lower: lower, cache: view.NewCache(cfg)}
	f.loop = &fs.LoopbackRoot{Path: lower, Dev: uint64(st.Dev)}
	f.loop.NewNode = func(rd *fs.LoopbackRoot, _ *fs.Inode, _ string, _ *syscall.Stat_t) fs.InodeEmbedder {
		return &pnode{LoopbackNode: fs.LoopbackNode{RootData: rd}, fsys: f}
	}
	f.Root = f.loop.NewNode(f.loop, nil, "", &st)
	f.loop.RootNode = f.Root
	return f, nil
}

// MountOptions configures Mount.
type MountOptions struct {
	AllowOther bool
	ReadOnly   bool
	Debug      bool
	Timeout    time.Duration // kernel attr/entry cache lifetime
}

// Mount mounts f on mountpoint. It uses mount(2) directly (no fusermount) and
// advertises FUSE_EXPORT_SUPPORT so the mount can be exported by knfsd.
func (f *FS) Mount(mountpoint string, o MountOptions) (*fuse.Server, error) {
	if o.Timeout == 0 {
		o.Timeout = time.Second
	}
	t := o.Timeout
	mo := fuse.MountOptions{
		AllowOther:        o.AllowOther,
		DirectMount:       true,
		FsName:            "pdfcbzfs",
		Name:              "pdfcbzfs",
		MaxWrite:          1 << 20,
		ExtraCapabilities: fuse.CAP_EXPORT_SUPPORT,
		Debug:             o.Debug,
	}
	if o.ReadOnly {
		mo.Options = append(mo.Options, "ro")
	}
	return fs.Mount(mountpoint, f.Root, &fs.Options{
		MountOptions:    mo,
		ExternalNodeID:  true,
		RootStableAttr:  &fs.StableAttr{Ino: 1, Mode: syscall.S_IFDIR},
		EntryTimeout:    &t,
		AttrTimeout:     &t,
		NegativeTimeout: &t,
	})
}

// pnode is a passthrough node that knows about PDFs.
type pnode struct {
	fs.LoopbackNode
	fsys *FS
}

func (n *pnode) realDir() string {
	return filepath.Join(n.fsys.Lower, n.Path(n.fsys.loop.RootNode.EmbeddedInode()))
}

var _ = (fs.NodeLookuper)((*pnode)(nil))

func (n *pnode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	dir := n.realDir()
	real := filepath.Join(dir, name)
	var st syscall.Stat_t
	if err := syscall.Lstat(real, &st); err == nil {
		if _, ok := view.IsPDFName(name); ok && st.Mode&syscall.S_IFMT == syscall.S_IFREG {
			if n.fsys.cache.Probe(real, &st) != nil {
				return nil, syscall.ENOENT // the PDF is only visible as CBZ
			}
		}
		return n.LoopbackNode.Lookup(ctx, name, out)
	}
	if stem, ok := view.IsCBZName(name); ok {
		if p := n.fsys.findPDF(dir, stem); p != nil && !p.Folder {
			return n.newVFile(ctx, p, 0, out), 0
		}
	}
	if p := n.fsys.findPDF(dir, name); p != nil && p.Folder {
		return n.newVDir(ctx, p, out), 0
	}
	return nil, syscall.ENOENT
}

func (f *FS) findPDF(dir, stem string) *view.PDF {
	for _, ext := range []string{".pdf", ".PDF", ".Pdf"} {
		path := filepath.Join(dir, stem+ext)
		var st syscall.Stat_t
		if syscall.Lstat(path, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
			continue
		}
		if p := f.cache.Probe(path, &st); p != nil {
			return p
		}
	}
	return nil
}

// visible is one directory entry as seen through the mount.
type visible struct {
	name string
	mode uint32
	ino  uint64
	pdf  *view.PDF // set for virtual directories
}

func (f *FS) list(dir string) ([]visible, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	exist := make(map[string]bool, len(ents))
	for _, e := range ents {
		exist[e.Name()] = true
	}
	out := make([]visible, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		name := e.Name()
		if _, isPDF := view.IsPDFName(name); isPDF && st.Mode&syscall.S_IFMT == syscall.S_IFREG {
			if p := f.cache.Probe(filepath.Join(dir, name), st); p != nil {
				if p.Folder {
					if !exist[p.DirName()] {
						out = append(out, visible{p.DirName(), syscall.S_IFDIR, p.DirIno(), p})
					}
				} else if !exist[p.SingleName()] {
					out = append(out, visible{p.SingleName(), syscall.S_IFREG, p.SingleIno(), nil})
				}
				continue
			}
		}
		out = append(out, visible{name, st.Mode & syscall.S_IFMT, st.Ino, nil})
	}
	return out, nil
}

// go-fuse's loopback implements OpendirHandle, which the bridge prefers over
// Readdir, so the listing has to be overridden here (not in Readdir alone).
var _ = (fs.NodeOpendirHandler)((*pnode)(nil))

func (n *pnode) OpendirHandle(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	vis, err := n.fsys.list(n.realDir())
	if err != nil {
		return nil, 0, fs.ToErrno(err)
	}
	ents := make([]fuse.DirEntry, len(vis))
	for i, v := range vis {
		ents[i] = fuse.DirEntry{Name: v.name, Mode: v.mode, Ino: v.ino}
	}
	return &dirHandle{ents: ents}, 0, 0
}

var _ = (fs.NodeReaddirer)((*pnode)(nil))

func (n *pnode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	h, _, errno := n.OpendirHandle(ctx, 0)
	if errno != 0 {
		return nil, errno
	}
	return fs.NewListDirStream(h.(*dirHandle).ents), 0
}

// dirHandle serves a precomputed listing.
type dirHandle struct {
	ents []fuse.DirEntry
	pos  int
}

var _ = (fs.FileReaddirenter)((*dirHandle)(nil))
var _ = (fs.FileSeekdirer)((*dirHandle)(nil))

func (d *dirHandle) Readdirent(ctx context.Context) (*fuse.DirEntry, syscall.Errno) {
	if d.pos >= len(d.ents) {
		return nil, 0
	}
	e := d.ents[d.pos]
	d.pos++
	return &e, 0
}

func (d *dirHandle) Seekdir(ctx context.Context, off uint64) syscall.Errno {
	if off > uint64(len(d.ents)) {
		off = uint64(len(d.ents))
	}
	d.pos = int(off)
	return 0
}

var _ = (fs.NodeLookupNoder)((*pnode)(nil))

// LookupNode resolves a bare node ID, which the kernel does when an NFS client
// presents a file handle that predates this process (e.g. after a restart).
// It finds the entry by walking the tree, then re-walks the path with Lookup so
// the inode ends up linked under its real parents.
func (n *pnode) LookupNode(ctx context.Context, id uint64, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path, ok := n.fsys.findIno(id)
	if !ok {
		return nil, syscall.ESTALE
	}
	cur := n.EmbeddedInode()
	for _, comp := range path {
		if ch := cur.GetChild(comp); ch != nil {
			cur = ch
			continue
		}
		lk, ok := cur.Operations().(fs.NodeLookuper)
		if !ok {
			return nil, syscall.ESTALE
		}
		ch, errno := lk.Lookup(ctx, comp, out)
		if errno != 0 {
			return nil, errno
		}
		cur.AddChild(comp, ch, false)
		cur = ch
	}
	if cur.StableAttr().Ino != id {
		return nil, syscall.ESTALE
	}
	if ga, ok := cur.Operations().(fs.NodeGetattrer); ok {
		var ao fuse.AttrOut
		if ga.Getattr(ctx, nil, &ao) == 0 {
			out.Attr = ao.Attr
		}
	}
	return cur, 0
}

// findIno returns the visible path of the entry with the given node ID. The
// map is rebuilt (at most every 10s) when the ID is unknown.
func (f *FS) findIno(id uint64) ([]string, bool) {
	f.idxMu.Lock()
	defer f.idxMu.Unlock()
	if p, ok := f.idx[id]; ok {
		return p, true
	}
	if f.idx != nil && time.Since(f.idxBuilt) < 10*time.Second {
		return nil, false
	}
	m := map[uint64][]string{}
	f.walk(f.Lower, nil, m, 0)
	f.idx, f.idxBuilt = m, time.Now()
	p, ok := m[id]
	return p, ok
}

func (f *FS) walk(dir string, prefix []string, m map[uint64][]string, depth int) {
	if depth > 64 {
		return
	}
	vis, err := f.list(dir)
	if err != nil {
		return
	}
	for _, v := range vis {
		path := append(append([]string{}, prefix...), v.name)
		m[v.ino] = path
		switch {
		case v.pdf != nil:
			for i := range v.pdf.Parts {
				m[v.pdf.PartIno(i)] = append(append([]string{}, path...), v.pdf.PartName(i))
			}
		case v.mode == syscall.S_IFDIR:
			f.walk(filepath.Join(dir, v.name), path, m, depth+1)
		}
	}
}

// ---- virtual regular file (one CBZ) ----

type vfile struct {
	fs.Inode
	pdf  *view.PDF
	part int
}

func (n *pnode) newVFile(ctx context.Context, p *view.PDF, part int, out *fuse.EntryOut) *fs.Inode {
	return newVFile(ctx, n.EmbeddedInode(), p, part, out)
}

func newVFile(ctx context.Context, parent *fs.Inode, p *view.PDF, part int, out *fuse.EntryOut) *fs.Inode {
	v := &vfile{pdf: p, part: part}
	ino := p.PartIno(part)
	if !p.Folder {
		ino = p.SingleIno()
	}
	v.fill(&out.Attr, ino)
	return parent.NewInode(ctx, v, fs.StableAttr{Mode: syscall.S_IFREG, Ino: ino, Gen: p.Gen()})
}

func (v *vfile) fill(a *fuse.Attr, ino uint64) {
	p := v.pdf
	size := uint64(p.Parts[v.part].Size())
	a.Ino = ino
	a.Mode = syscall.S_IFREG | 0o444
	a.Size = size
	a.Blocks = (size + 511) / 512
	a.Blksize = 4096
	a.Nlink = 1
	a.Uid, a.Gid = p.Uid, p.Gid
	a.SetTimes(&p.Mod, &p.Mod, &p.Mod)
}

var _ = (fs.NodeGetattrer)((*vfile)(nil))

func (v *vfile) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	ino := v.pdf.PartIno(v.part)
	if !v.pdf.Folder {
		ino = v.pdf.SingleIno()
	}
	v.fill(&out.Attr, ino)
	return 0
}

var _ = (fs.NodeOpener)((*vfile)(nil))

func (v *vfile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC|syscall.O_APPEND) != 0 {
		return nil, 0, syscall.EROFS
	}
	f, err := os.Open(v.pdf.Path)
	if err != nil {
		return nil, 0, fs.ToErrno(err)
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() != v.pdf.Size || !fi.ModTime().Equal(v.pdf.Mod) {
		f.Close()
		return nil, 0, syscall.ESTALE // the PDF changed under us
	}
	return &vhandle{f: f, pdf: v.pdf, part: v.part}, fuse.FOPEN_KEEP_CACHE, 0
}

type vhandle struct {
	f    *os.File
	pdf  *view.PDF
	part int
}

var _ = (fs.FileReader)((*vhandle)(nil))
var _ = (fs.FileReleaser)((*vhandle)(nil))

func (h *vhandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.pdf.Parts[h.part].ReadAt(h.f, dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *vhandle) Release(ctx context.Context) syscall.Errno {
	return fs.ToErrno(h.f.Close())
}

// ---- virtual directory (a PDF with several parts) ----

type vdir struct {
	fs.Inode
	pdf *view.PDF
}

func (n *pnode) newVDir(ctx context.Context, p *view.PDF, out *fuse.EntryOut) *fs.Inode {
	d := &vdir{pdf: p}
	d.fill(&out.Attr)
	return n.NewInode(ctx, d, fs.StableAttr{Mode: syscall.S_IFDIR, Ino: p.DirIno(), Gen: p.Gen()})
}

func (d *vdir) fill(a *fuse.Attr) {
	p := d.pdf
	a.Ino = p.DirIno()
	a.Mode = syscall.S_IFDIR | 0o555
	a.Size = 4096
	a.Blksize = 4096
	a.Nlink = 2
	a.Uid, a.Gid = p.Uid, p.Gid
	a.SetTimes(&p.Mod, &p.Mod, &p.Mod)
}

var _ = (fs.NodeGetattrer)((*vdir)(nil))

func (d *vdir) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	d.fill(&out.Attr)
	return 0
}

var _ = (fs.NodeLookuper)((*vdir)(nil))

func (d *vdir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	for i := range d.pdf.Parts {
		if d.pdf.PartName(i) == name {
			return newVFile(ctx, d.EmbeddedInode(), d.pdf, i, out), 0
		}
	}
	return nil, syscall.ENOENT
}

var _ = (fs.NodeReaddirer)((*vdir)(nil))

func (d *vdir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	ents := make([]fuse.DirEntry, len(d.pdf.Parts))
	for i := range d.pdf.Parts {
		ents[i] = fuse.DirEntry{Name: d.pdf.PartName(i), Mode: syscall.S_IFREG, Ino: d.pdf.PartIno(i)}
	}
	return fs.NewListDirStream(ents), 0
}
