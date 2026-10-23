package pdfidx

import "fmt"

// pngSpecFor maps a Flate image dictionary onto a PNG, or explains why not.
// Supported: DeviceGray/RGB (also Cal*, ICCBased with N=1 or 3) at 1/2/4/8
// bits (RGB: 8), and Indexed over gray/RGB. Anything else (CMYK, Lab,
// Separation, 16-bit, alpha) is reported as unsupported. An /ImageMask is
// treated as black ink on a white page.
func (d *doc) pngSpecFor(id dict, num, gen int, parms dict, mask, invert bool) (*pngSpec, error) {
	w, h := intOf(id, "Width", 0), intOf(id, "Height", 0)
	bpc := intOf(id, "BitsPerComponent", 0)
	if mask {
		bpc = 1 // an image mask is always 1 bit; the key is optional
	}
	if w <= 0 || h <= 0 || w*h > 1<<28 {
		return nil, fmt.Errorf("bad image size %dx%d", w, h)
	}
	switch bpc {
	case 1, 2, 4, 8:
	default:
		return nil, fmt.Errorf("%d bits per component", bpc)
	}
	s := &pngSpec{w: int(w), h: int(h), bpc: int(bpc), parms: parms, invert: invert}
	if mask {
		// Sample 0 paints (black, the usual fill) and 1 leaves the page
		// white, which is exactly 1-bit DeviceGray.
		s.ctype, s.colors = 0, 1
		return s, nil
	}
	ct, comps, base, err := d.colorSpace(id["ColorSpace"], 0)
	if err != nil {
		return nil, err
	}
	switch ct {
	case 'G':
		s.ctype, s.colors = 0, 1
	case 'R':
		if bpc != 8 {
			return nil, fmt.Errorf("RGB at %d bits", bpc)
		}
		s.ctype, s.colors = 2, 3
	case 'I':
		s.ctype, s.colors = 3, 1
		pal, err := d.palette(base, comps, num, gen)
		if err != nil {
			return nil, err
		}
		s.palette = pal
	}
	return s, nil
}

// colorSpace classifies a colour space: 'G' gray, 'R' RGB, 'I' indexed. For
// indexed it returns the raw array in base and the base's component count in
// comps.
func (d *doc) colorSpace(v any, depth int) (kind byte, comps int, indexed array, err error) {
	if depth > 4 {
		return 0, 0, nil, fmt.Errorf("colour space nesting")
	}
	v, err = d.deref(v)
	if err != nil {
		return 0, 0, nil, err
	}
	switch t := v.(type) {
	case name:
		switch t {
		case "DeviceGray", "G", "CalGray":
			return 'G', 1, nil, nil
		case "DeviceRGB", "RGB", "CalRGB":
			return 'R', 3, nil, nil
		}
		return 0, 0, nil, fmt.Errorf("colour space /%s", t)
	case array:
		if len(t) == 0 {
			return 0, 0, nil, fmt.Errorf("empty colour space")
		}
		head, _ := t[0].(name)
		switch head {
		case "CalGray":
			return 'G', 1, nil, nil
		case "CalRGB":
			return 'R', 3, nil, nil
		case "ICCBased":
			if len(t) < 2 {
				return 0, 0, nil, fmt.Errorf("bad ICCBased")
			}
			pd, err := d.derefDict(t[1])
			if err != nil {
				return 0, 0, nil, err
			}
			switch intOf(pd, "N", 0) {
			case 1:
				return 'G', 1, nil, nil
			case 3:
				return 'R', 3, nil, nil
			}
			return 0, 0, nil, fmt.Errorf("ICCBased N=%d", intOf(pd, "N", 0))
		case "Indexed", "I":
			if len(t) != 4 {
				return 0, 0, nil, fmt.Errorf("bad Indexed")
			}
			bk, bc, _, err := d.colorSpace(t[1], depth+1)
			if err != nil || bk == 'I' {
				return 0, 0, nil, fmt.Errorf("Indexed base")
			}
			return 'I', bc, t, nil
		}
		return 0, 0, nil, fmt.Errorf("colour space /%s", head)
	}
	return 0, 0, nil, fmt.Errorf("colour space %T", v)
}

// palette returns the PLTE bytes of an Indexed colour space whose base has
// comps components (1: gray replicated to RGB, 3: RGB).
func (d *doc) palette(ix array, comps, num, gen int) ([]byte, error) {
	hival, _ := ix[2].(int64)
	if hival < 0 || hival > 255 {
		return nil, fmt.Errorf("bad Indexed hival %d", hival)
	}
	entries := int(hival) + 1
	var lut []byte
	switch t := ix[3].(type) {
	case pstring:
		lut = d.plainString(num, gen, true, t)
	case ref:
		o, g, so, _, err := d.resolve(t.num)
		if err != nil {
			return nil, err
		}
		switch ov := o.(type) {
		case pstring:
			lut = d.plainString(t.num, g, d.xref[t.num].typ == 1, ov)
		case dict:
			if so < 0 {
				return nil, fmt.Errorf("Indexed lookup stream")
			}
			lut, err = d.streamData(t.num, g, ov, so)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("Indexed lookup %T", o)
		}
	default:
		return nil, fmt.Errorf("Indexed lookup %T", t)
	}
	if len(lut) < entries*comps {
		return nil, fmt.Errorf("short Indexed lookup (%d < %d)", len(lut), entries*comps)
	}
	pal := make([]byte, 0, entries*3)
	for i := 0; i < entries; i++ {
		if comps == 1 {
			g := lut[i]
			pal = append(pal, g, g, g)
		} else {
			pal = append(pal, lut[i*3:i*3+3]...)
		}
	}
	return pal, nil
}

// plainString decrypts a string object when the document is encrypted. direct
// strings live in an in-file object; strings inside object streams are not
// encrypted individually.
func (d *doc) plainString(num, gen int, inFile bool, s pstring) []byte {
	if d.crypt == nil || !inFile || d.crypt.identity() {
		return s
	}
	out, err := d.crypt.decryptStream(num, gen, s)
	if err != nil {
		return s
	}
	return out
}
