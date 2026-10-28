package render

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// Config configures a Service. The zero value finds the helper itself, uses
// no disk cache and runs up to min(2, CPUs) renders at once.
type Config struct {
	Backend  Backend // nil: look for the helper (again, with backoff, if absent)
	CacheDir string
	MaxCache int64
	Workers  int
	// PageTimeout bounds one helper run (default 90s).
	PageTimeout time.Duration

	// for tests
	Now   func() time.Time
	Sleep func(time.Duration)
}

// Service renders pages for the filesystem: it knows whether a renderer is
// usable (re-checked with exponential backoff), shares work between callers
// asking for the same page, caps concurrent helper processes, retries
// transient failures and keeps results on disk.
type Service struct {
	cfg   Config
	cache *Cache
	sem   chan struct{}

	mu        sync.Mutex
	be        Backend
	ok        bool
	nextCheck time.Time
	delay     time.Duration
	lastErr   error

	fmu    sync.Mutex
	flight map[string]*call
}

type call struct {
	done chan struct{}
	b    []byte
	err  error
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Workers <= 0 {
		// every worker is a PDFium process of some tens of MB: stay small
		cfg.Workers = min(2, max(1, runtime.NumCPU()))
	}
	if cfg.PageTimeout <= 0 {
		cfg.PageTimeout = 90 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	s := &Service{cfg: cfg, sem: make(chan struct{}, cfg.Workers), flight: map[string]*call{}}
	if cfg.CacheDir != "" {
		s.cache = NewCache(cfg.CacheDir, cfg.MaxCache)
	}
	return s
}

// checkDelays is the backoff between availability checks while the renderer
// is missing or broken; the last value repeats.
var checkDelays = []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

func (s *Service) backend() Backend {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ok {
		return s.be
	}
	now := s.cfg.Now()
	if now.Before(s.nextCheck) {
		return nil
	}
	be := s.cfg.Backend
	if be == nil {
		if h := FindHelper(); h != nil {
			be = h
		}
	}
	var err error
	if be == nil {
		err = errors.New("render helper pdfcbzfs-render not found")
	} else if err = be.Check(); err == nil {
		s.be, s.ok, s.delay, s.lastErr = be, true, 0, nil
		return be
	}
	s.lastErr = err
	i := 0
	for i < len(checkDelays)-1 && checkDelays[i] <= s.delay {
		i++
	}
	s.delay = checkDelays[i]
	s.nextCheck = now.Add(s.delay)
	return nil
}

// Available reports whether pages can be rendered right now.
func (s *Service) Available() bool { return s != nil && s.backend() != nil }

// Why says why the renderer is unavailable.
func (s *Service) Why() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Service) invalidate() {
	s.mu.Lock()
	s.ok = false
	s.nextCheck = s.cfg.Now().Add(checkDelays[0])
	s.delay = 0
	s.mu.Unlock()
}

// PageKey identifies one page to render.
type PageKey struct {
	FileID   string // identity of the file version (dev, inode, size, mtime)
	Path     string
	Password []byte
	Page     int // 0-based
	Budget   int64
	DPI      float64
	MaxSide  int
}

func (k PageKey) cacheKey() string {
	return fmt.Sprintf("v1|%s|%d|%d|%g|%d", k.FileID, k.Page, k.Budget, k.DPI, k.MaxSide)
}

// Page returns the rendered page as a JPEG of exactly k.Budget bytes.
func (s *Service) Page(ctx context.Context, k PageKey) ([]byte, error) {
	key := k.cacheKey()
	if b, ok := s.cache.Get(key, k.Budget); ok {
		return b, nil
	}
	s.fmu.Lock()
	if c, ok := s.flight[key]; ok {
		s.fmu.Unlock()
		select {
		case <-c.done:
			return c.b, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	s.flight[key] = c
	s.fmu.Unlock()

	c.b, c.err = s.render(ctx, k, key)
	s.fmu.Lock()
	delete(s.flight, key)
	s.fmu.Unlock()
	close(c.done)
	return c.b, c.err
}

// retryDelays are the waits before each attempt of a transient failure.
var retryDelays = []time.Duration{0, 400 * time.Millisecond, 1500 * time.Millisecond}

func (s *Service) render(ctx context.Context, k PageKey, key string) ([]byte, error) {
	be := s.backend()
	if be == nil {
		return nil, &Error{Msg: fmt.Sprintf("renderer unavailable: %v", s.Why())}
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var last error
	for _, d := range retryDelays {
		if d > 0 {
			s.cfg.Sleep(d)
		}
		rctx, cancel := context.WithTimeout(ctx, s.cfg.PageTimeout)
		r, err := be.Render(rctx, Request{Path: k.Path, Password: k.Password, Page: k.Page, DPI: k.DPI, MaxSide: k.MaxSide})
		cancel()
		if err == nil {
			b, ferr := FitJPEG(r, k.Budget)
			if ferr != nil {
				return nil, ferr
			}
			s.cache.Put(key, b)
			return b, nil
		}
		last = err
		var re *Error
		if errors.As(err, &re) && re.Permanent {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	s.invalidate() // let the next caller re-check the helper
	return nil, last
}
