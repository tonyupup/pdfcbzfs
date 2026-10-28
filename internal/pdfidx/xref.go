package pdfidx

import (
	"bytes"
	"strconv"
)

type section struct {
	entries map[int]xentry
	order   []int
	trailer dict
}

func (d *doc) loadXref() error {
	// startxref is normally in the last bytes, but some producers pad the file
	// with zeros or garbage after %%EOF: widen the window before giving up.
	var tail []byte
	i := -1
	for _, n := range []int64{2048, 64 << 10, 1 << 20} {
		if n > d.size {
			n = d.size
		}
		var err error
		tail, err = d.readAt(d.size-n, int(n))
		if err != nil {
			return err
		}
		if i = bytes.LastIndex(tail, []byte("startxref")); i >= 0 || n == d.size {
			break
		}
	}
	if i < 0 {
		last := tail
		if len(last) > 2048 {
			last = last[len(last)-2048:]
		}
		if len(bytes.Trim(last, "\x00")) == 0 {
			return unsupported("no startxref: the end of the file is all zeros (incomplete or still downloading?)")
		}
		return unsupported("no startxref")
	}
	p := &parser{b: append(append([]byte{}, tail[i+9:]...), ' ')}
	off, err := p.readInt()
	if err != nil {
		return unsupported("bad startxref: %v", err)
	}

	d.xref = map[int]xentry{}
	visited := map[int64]bool{}
	first := true
	for off >= 0 {
		if visited[off] {
			break
		}
		visited[off] = true
		sec, err := d.readSection(off)
		if err != nil {
			return err
		}
		d.merge(sec)
		if first {
			d.trailer = sec.trailer
			first = false
		}
		if xs, ok := sec.trailer["XRefStm"].(int64); ok && !visited[xs] {
			visited[xs] = true
			s2, err := d.readSection(xs)
			if err != nil {
				return err
			}
			d.merge(s2)
		}
		prev, ok := sec.trailer["Prev"].(int64)
		if !ok {
			if f, okf := sec.trailer["Prev"].(float64); okf {
				prev, ok = int64(f), true
			}
		}
		if !ok {
			break
		}
		off = prev
	}
	if d.trailer == nil {
		return unsupported("no trailer")
	}
	return nil
}

func (d *doc) merge(s *section) {
	for _, num := range s.order {
		if _, ok := d.xref[num]; !ok {
			d.xref[num] = s.entries[num]
		}
	}
}

func (d *doc) readSection(off int64) (*section, error) {
	head, err := d.readAt(off, 32)
	if err != nil {
		return nil, err
	}
	p := &parser{b: head}
	p.skipWS()
	if bytes.HasPrefix(head[p.pos:], []byte("xref")) {
		return d.readTable(off)
	}
	return d.readXrefStream(off)
}

func (d *doc) readTable(off int64) (*section, error) {
	sec := &section{entries: map[int]xentry{}}
	err := d.parseAt(off, func(p *parser) error {
		sec.entries = map[int]xentry{}
		sec.order = sec.order[:0]
		if err := p.expectKeyword("xref"); err != nil {
			return err
		}
		for {
			p.skipWS()
			if p.pos >= len(p.b) {
				return errShort
			}
			if p.b[p.pos] == 't' {
				if err := p.expectKeyword("trailer"); err != nil {
					return err
				}
				o, err := p.parseObject()
				if err != nil {
					return err
				}
				dc, ok := o.(dict)
				if !ok {
					return strconv.ErrSyntax
				}
				sec.trailer = dc
				return nil
			}
			start, err := p.readInt()
			if err != nil {
				return err
			}
			count, err := p.readInt()
			if err != nil {
				return err
			}
			for i := int64(0); i < count; i++ {
				o, err := p.readInt()
				if err != nil {
					return err
				}
				g, err := p.readInt()
				if err != nil {
					return err
				}
				p.skipWS()
				if p.pos >= len(p.b)-1 {
					return errShort
				}
				kind := p.b[p.pos]
				p.pos++
				num := int(start + i)
				switch kind {
				case 'n':
					sec.entries[num] = xentry{typ: 1, a: o, b: g}
				case 'f':
					sec.entries[num] = xentry{typ: 0}
				default:
					return strconv.ErrSyntax
				}
				sec.order = append(sec.order, num)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return sec, nil
}

func (d *doc) readXrefStream(off int64) (*section, error) {
	_, gen, o, so, err := d.readObjAt(off)
	if err != nil {
		return nil, err
	}
	dc, ok := o.(dict)
	if !ok || so < 0 || name2(dc["Type"]) != "XRef" {
		return nil, unsupported("expected xref stream at %d", off)
	}
	num := 0
	_ = gen
	data, err := d.streamData(num, 0, dc, so)
	if err != nil {
		return nil, err
	}
	wa, _ := dc["W"].(array)
	if len(wa) != 3 {
		return nil, unsupported("bad xref /W")
	}
	var w [3]int
	for i, v := range wa {
		n, _ := v.(int64)
		if n < 0 || n > 8 {
			return nil, unsupported("bad xref /W")
		}
		w[i] = int(n)
	}
	rowLen := w[0] + w[1] + w[2]
	if rowLen == 0 {
		return nil, unsupported("empty xref /W")
	}
	size, _ := dc["Size"].(int64)
	idx := []int64{0, size}
	if ia, ok := dc["Index"].(array); ok {
		idx = idx[:0]
		for _, v := range ia {
			n, _ := v.(int64)
			idx = append(idx, n)
		}
	}
	sec := &section{entries: map[int]xentry{}, trailer: dc}
	pos := 0
	field := func(n int) int64 {
		var v int64
		for i := 0; i < n; i++ {
			v = v<<8 | int64(data[pos])
			pos++
		}
		return v
	}
	for i := 0; i+1 < len(idx); i += 2 {
		for j := int64(0); j < idx[i+1]; j++ {
			if pos+rowLen > len(data) {
				return nil, unsupported("xref stream truncated")
			}
			t := int64(1)
			if w[0] > 0 {
				t = field(w[0])
			}
			a := field(w[1])
			b := field(w[2])
			n := int(idx[i] + j)
			switch t {
			case 0:
				sec.entries[n] = xentry{typ: 0}
			case 1:
				sec.entries[n] = xentry{typ: 1, a: a, b: b}
			case 2:
				sec.entries[n] = xentry{typ: 2, a: a, b: b}
			default:
				continue
			}
			sec.order = append(sec.order, n)
		}
	}
	return sec, nil
}
