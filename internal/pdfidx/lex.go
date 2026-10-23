package pdfidx

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// PDF object model (only what the indexer needs).
type (
	name    string
	ref     struct{ num, gen int }
	dict    map[string]any
	array   []any
	pstring []byte
	keyword string
)

// errShort means the buffer ended in the middle of an object; the caller may
// retry with a larger buffer.
var errShort = errors.New("pdfidx: truncated buffer")

type parser struct {
	b   []byte
	pos int
}

func isWS(c byte) bool {
	return c == 0 || c == 9 || c == 10 || c == 12 || c == 13 || c == 32
}

func isDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func (p *parser) skipWS() {
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		if isWS(c) {
			p.pos++
		} else if c == '%' {
			for p.pos < len(p.b) && p.b[p.pos] != '\n' && p.b[p.pos] != '\r' {
				p.pos++
			}
		} else {
			return
		}
	}
}

// token reads a run of regular characters.
func (p *parser) token() string {
	s := p.pos
	for p.pos < len(p.b) && !isWS(p.b[p.pos]) && !isDelim(p.b[p.pos]) {
		p.pos++
	}
	return string(p.b[s:p.pos])
}

func (p *parser) readInt() (int64, error) {
	p.skipWS()
	if p.pos >= len(p.b) {
		return 0, errShort
	}
	s := p.pos
	t := p.token()
	if p.pos >= len(p.b) {
		return 0, errShort
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		p.pos = s
		return 0, fmt.Errorf("pdfidx: expected integer, got %q", t)
	}
	return n, nil
}

func (p *parser) expectKeyword(kw string) error {
	p.skipWS()
	if p.pos >= len(p.b) {
		return errShort
	}
	s := p.pos
	t := p.token()
	if p.pos >= len(p.b) {
		return errShort
	}
	if t != kw {
		p.pos = s
		return fmt.Errorf("pdfidx: expected %q, got %q", kw, t)
	}
	return nil
}

func (p *parser) parseObject() (any, error) {
	p.skipWS()
	if p.pos >= len(p.b) {
		return nil, errShort
	}
	switch c := p.b[p.pos]; {
	case c == '<':
		if p.pos+1 >= len(p.b) {
			return nil, errShort
		}
		if p.b[p.pos+1] == '<' {
			p.pos += 2
			return p.parseDict()
		}
		return p.parseHex()
	case c == '[':
		p.pos++
		var a array
		for {
			p.skipWS()
			if p.pos >= len(p.b) {
				return nil, errShort
			}
			if p.b[p.pos] == ']' {
				p.pos++
				return a, nil
			}
			o, err := p.parseObject()
			if err != nil {
				return nil, err
			}
			a = append(a, o)
		}
	case c == '/':
		p.pos++
		return p.parseName()
	case c == '(':
		return p.parseLiteral()
	case c == ')' || c == ']' || c == '>' || c == '{' || c == '}':
		return nil, fmt.Errorf("pdfidx: unexpected %q at %d", c, p.pos)
	}
	// number, ref or keyword
	start := p.pos
	t := p.token()
	if p.pos >= len(p.b) {
		return nil, errShort
	}
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		// maybe "N G R"
		save := p.pos
		if g, err2 := p.readInt(); err2 == nil {
			if err3 := p.expectKeyword("R"); err3 == nil {
				return ref{int(n), int(g)}, nil
			} else if errors.Is(err3, errShort) {
				return nil, errShort
			}
		} else if errors.Is(err2, errShort) {
			return nil, errShort
		}
		p.pos = save
		return n, nil
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f, nil
	}
	switch t {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	case "":
		p.pos = start + 1
		return nil, fmt.Errorf("pdfidx: bad token at %d", start)
	}
	return keyword(t), nil
}

func (p *parser) parseName() (name, error) {
	s := p.pos
	for p.pos < len(p.b) && !isWS(p.b[p.pos]) && !isDelim(p.b[p.pos]) {
		p.pos++
	}
	if p.pos >= len(p.b) {
		return "", errShort
	}
	raw := p.b[s:p.pos]
	if bytes.IndexByte(raw, '#') < 0 {
		return name(raw), nil
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i+2 < len(raw)+0 && i+2 <= len(raw)-1+1 {
			if v, err := strconv.ParseUint(string(raw[i+1:i+3]), 16, 8); err == nil {
				out = append(out, byte(v))
				i += 2
				continue
			}
		}
		out = append(out, raw[i])
	}
	return name(out), nil
}

func (p *parser) parseDict() (dict, error) {
	d := dict{}
	for {
		p.skipWS()
		if p.pos+1 >= len(p.b) {
			return nil, errShort
		}
		if p.b[p.pos] == '>' && p.b[p.pos+1] == '>' {
			p.pos += 2
			return d, nil
		}
		if p.b[p.pos] != '/' {
			return nil, fmt.Errorf("pdfidx: dict key expected at %d", p.pos)
		}
		p.pos++
		k, err := p.parseName()
		if err != nil {
			return nil, err
		}
		v, err := p.parseObject()
		if err != nil {
			return nil, err
		}
		d[string(k)] = v
	}
}

func (p *parser) parseHex() (pstring, error) {
	p.pos++ // '<'
	var out []byte
	var hi int = -1
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		p.pos++
		if c == '>' {
			if hi >= 0 {
				out = append(out, byte(hi<<4))
			}
			return out, nil
		}
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		case isWS(c):
			continue
		default:
			return nil, fmt.Errorf("pdfidx: bad hex digit %q", c)
		}
		if hi < 0 {
			hi = v
		} else {
			out = append(out, byte(hi<<4|v))
			hi = -1
		}
	}
	return nil, errShort
}

func (p *parser) parseLiteral() (pstring, error) {
	p.pos++ // '('
	depth := 1
	var out []byte
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		p.pos++
		switch c {
		case '(':
			depth++
			out = append(out, c)
		case ')':
			depth--
			if depth == 0 {
				return out, nil
			}
			out = append(out, c)
		case '\\':
			if p.pos >= len(p.b) {
				return nil, errShort
			}
			e := p.b[p.pos]
			p.pos++
			switch e {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case '\r':
				if p.pos < len(p.b) && p.b[p.pos] == '\n' {
					p.pos++
				}
			case '\n':
			case '0', '1', '2', '3', '4', '5', '6', '7':
				v := int(e - '0')
				for i := 0; i < 2 && p.pos < len(p.b) && p.b[p.pos] >= '0' && p.b[p.pos] <= '7'; i++ {
					v = v*8 + int(p.b[p.pos]-'0')
					p.pos++
				}
				out = append(out, byte(v))
			default:
				out = append(out, e)
			}
		default:
			out = append(out, c)
		}
	}
	return nil, errShort
}
