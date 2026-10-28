package render

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Cache keeps rendered pages on disk so that a restart does not render a book
// again. Entries are exact-size JPEGs; an entry of the wrong length is
// discarded. Old entries go first when the directory outgrows its limit.
type Cache struct {
	dir string
	max int64

	mu      sync.Mutex
	written int64
}

// NewCache returns a cache in dir limited to about max bytes, or nil when dir
// cannot be created (rendering then works without a cache).
func NewCache(dir string, max int64) *Cache {
	if dir == "" || os.MkdirAll(dir, 0o755) != nil {
		return nil
	}
	c := &Cache{dir: dir, max: max}
	go c.gc()
	return c
}

func (c *Cache) path(key string) string {
	h := sha1.Sum([]byte(key))
	name := hex.EncodeToString(h[:])
	return filepath.Join(c.dir, name[:2], name[2:]+".jpg")
}

// Get returns the entry for key when it has exactly size bytes.
func (c *Cache) Get(key string, size int64) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	p := c.path(key)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	if int64(len(b)) != size {
		os.Remove(p)
		return nil, false
	}
	return b, true
}

// Put stores an entry (atomically: write a temp file, then rename).
func (c *Cache) Put(key string, b []byte) {
	if c == nil {
		return
	}
	p := c.path(key)
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), p) != nil {
		os.Remove(tmp.Name())
		return
	}
	c.mu.Lock()
	c.written += int64(len(b))
	due := c.written > c.max/16
	if due {
		c.written = 0
	}
	c.mu.Unlock()
	if due {
		go c.gc()
	}
}

// gc deletes the oldest entries until the cache is below 80% of its limit.
func (c *Cache) gc() {
	type file struct {
		path string
		size int64
		mod  int64
	}
	var files []file
	var total int64
	filepath.Walk(c.dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		files = append(files, file{p, fi.Size(), fi.ModTime().UnixNano()})
		total += fi.Size()
		return nil
	})
	if total <= c.max {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod < files[j].mod })
	for _, f := range files {
		if total <= c.max/10*8 {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
