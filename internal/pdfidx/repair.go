package pdfidx

import (
	"bytes"
	"strconv"
	"sync/atomic"
)

// Some producers leave xref offsets that are a little off: bytes were inserted
// or removed after the table was written, so every object behind that point is
// recorded N bytes too early or too late, and a /Prev or startxref pointer may
// point into the middle of another object. The objects are all there, just not
// where the table says. These helpers look near the recorded position, learn
// the shift, and try it first for the next object.
const (
	repairBefore = 8 << 10  // how far before the recorded offset to look
	repairAfter  = 64 << 10 // and how far after (inserted bytes push objects later)
	maxShift     = 1 << 20  // a shift larger than this is not learned
)

// repairState is embedded in doc.
type repairState struct {
	shift   atomic.Int64
	repairs atomic.Int64
	fix     map[int]int64 // object number -> corrected offset (-1: could not be found); guarded by doc.mu
}

// fixedOffset returns the corrected offset of an object found earlier.
func (d *doc) fixedOffset(num int) (int64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	off, ok := d.fix[num]
	return off, ok
}

func (d *doc) setFixed(num int, off int64) {
	d.mu.Lock()
	if d.fix == nil {
		d.fix = map[int]int64{}
	}
	d.fix[num] = off
	d.mu.Unlock()
}

// hasHeaderAt reports whether "num gen obj" starts exactly at off.
func (d *doc) hasHeaderAt(num int, off int64) bool {
	b, err := d.readAt(off, 48)
	if err != nil && len(b) == 0 {
		return false
	}
	return headerMatches(b, 0, num)
}

// headerMatches checks for "num <ws> gen <ws> obj" at b[i:].
func headerMatches(b []byte, i int, num int) bool {
	s := strconv.Itoa(num)
	if !bytes.HasPrefix(b[i:], []byte(s)) {
		return false
	}
	i += len(s)
	skip := func() bool {
		j := i
		for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n' || b[i] == 0 || b[i] == '\f') {
			i++
		}
		return i > j
	}
	if !skip() {
		return false
	}
	j := i
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		i++
	}
	if i == j || !skip() {
		return false
	}
	return bytes.HasPrefix(b[i:], []byte("obj")) && (i+3 >= len(b) || !isAlpha(b[i+3]))
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// findObject looks for the header of object num near the recorded offset.
func (d *doc) findObject(num int, off int64) (int64, bool) {
	if sh := d.shift.Load(); sh != 0 {
		if try := off + sh; try >= 0 && d.hasHeaderAt(num, try) {
			return try, true
		}
	}
	lo := max(0, off-repairBefore)
	buf, err := d.readAt(lo, int(off-lo)+repairAfter)
	if err != nil && len(buf) == 0 {
		return 0, false
	}
	s := []byte(strconv.Itoa(num))
	best, bestDist := int64(-1), int64(1)<<62
	for from := 0; from < len(buf); {
		i := bytes.Index(buf[from:], s)
		if i < 0 {
			break
		}
		i += from
		from = i + 1
		if i > 0 && buf[i-1] >= '0' && buf[i-1] <= '9' {
			continue // the tail of a longer number
		}
		if !headerMatches(buf, i, num) {
			continue
		}
		pos := lo + int64(i)
		if dist := abs64(pos - off); dist < bestDist {
			best, bestDist = pos, dist
		}
	}
	return best, best >= 0
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// learnShift remembers the offset error that just turned out to be right.
func (d *doc) learnShift(recorded, found int64) {
	if sh := found - recorded; sh != 0 && abs64(sh) < maxShift {
		d.shift.Store(sh)
	}
}

// findXrefNear looks for the "xref" keyword of a classic table near off, for a
// /Prev or startxref pointer that is slightly off.
func (d *doc) findXrefNear(off int64) (int64, bool) {
	lo := max(0, off-repairBefore)
	buf, err := d.readAt(lo, int(off-lo)+repairAfter)
	if err != nil && len(buf) == 0 {
		return 0, false
	}
	best, bestDist := int64(-1), int64(1)<<62
	for from := 0; from < len(buf); {
		i := bytes.Index(buf[from:], []byte("xref"))
		if i < 0 {
			break
		}
		i += from
		from = i + 1
		if i > 0 && buf[i-1] != '\n' && buf[i-1] != '\r' {
			continue // "startxref" and other words ending in xref
		}
		if i+4 < len(buf) && buf[i+4] != '\n' && buf[i+4] != '\r' && buf[i+4] != ' ' {
			continue
		}
		pos := lo + int64(i)
		if dist := abs64(pos - off); dist < bestDist {
			best, bestDist = pos, dist
		}
	}
	return best, best >= 0
}
