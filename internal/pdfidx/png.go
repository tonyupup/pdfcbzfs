package pdfidx

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/adler32"
	"hash/crc32"
	"io"
)

// pngSpec describes a Flate raw-sample image that is presented as a PNG.
type pngSpec struct {
	w, h, bpc int
	colors    int  // samples per pixel: 1 (gray, indexed) or 3 (RGB)
	ctype     byte // PNG colour type: 0 gray, 2 RGB, 3 indexed
	palette   []byte
	parms     dict // /DecodeParms (predictor), may be nil
	invert    bool // flip every sample bit (ImageMask with /Decode [1 0])
}

func (s *pngSpec) rowBytes() int { return (s.w*s.colors*s.bpc + 7) / 8 }

// pngSize is the exact length of buildPNG's output. The IDAT payload uses
// stored (uncompressed) deflate blocks, so the size depends only on the image
// geometry: stat() never has to decode a page.
func (s *pngSpec) pngSize() int64 {
	data := int64(s.h) * int64(s.rowBytes()+1)
	blocks := (data + 65534) / 65535
	if blocks == 0 {
		blocks = 1
	}
	idat := 2 + blocks*5 + data + 4
	size := int64(8+25) + 12 + idat + 12
	if len(s.palette) > 0 {
		size += 12 + int64(len(s.palette))
	}
	return size
}

func pngChunk(dst *bytes.Buffer, typ string, data []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(data)))
	dst.Write(n[:])
	dst.WriteString(typ)
	dst.Write(data)
	c := crc32.Update(0, crc32.IEEETable, []byte(typ))
	c = crc32.Update(c, crc32.IEEETable, data)
	binary.BigEndian.PutUint32(n[:], c)
	dst.Write(n[:])
}

// buildPNG wraps raw rows (h * rowBytes, already un-predicted) in a PNG.
func (s *pngSpec) buildPNG(raw []byte) ([]byte, error) {
	rb := s.rowBytes()
	if len(raw) < s.h*rb {
		return nil, unsupported("image data too short: %d < %d", len(raw), s.h*rb)
	}
	data := make([]byte, s.h*(rb+1))
	for y := 0; y < s.h; y++ {
		copy(data[y*(rb+1)+1:(y+1)*(rb+1)], raw[y*rb:(y+1)*rb])
	}
	var z bytes.Buffer
	z.Write([]byte{0x78, 0x01})
	rest := data
	for first := true; first || len(rest) > 0; first = false {
		n := len(rest)
		if n > 65535 {
			n = 65535
		}
		final := byte(0)
		if n == len(rest) {
			final = 1
		}
		z.Write([]byte{final, byte(n), byte(n >> 8), ^byte(n), ^byte(n >> 8)})
		z.Write(rest[:n])
		rest = rest[n:]
	}
	var ad [4]byte
	binary.BigEndian.PutUint32(ad[:], adler32.Checksum(data))
	z.Write(ad[:])

	var out bytes.Buffer
	out.Grow(int(s.pngSize()))
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	var ih [13]byte
	binary.BigEndian.PutUint32(ih[0:], uint32(s.w))
	binary.BigEndian.PutUint32(ih[4:], uint32(s.h))
	ih[8], ih[9] = byte(s.bpc), s.ctype
	pngChunk(&out, "IHDR", ih[:])
	if len(s.palette) > 0 {
		pngChunk(&out, "PLTE", s.palette)
	}
	pngChunk(&out, "IDAT", z.Bytes())
	pngChunk(&out, "IEND", nil)
	return out.Bytes(), nil
}

// decodeRaw inflates a Flate image stream and undoes its predictor.
func (s *pngSpec) decodeRaw(stream []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(stream))
	if err != nil {
		return nil, unsupported("flate: %v", err)
	}
	want := s.h * s.rowBytes()
	raw, err := io.ReadAll(io.LimitReader(zr, int64(want)*2+(1<<20)))
	if err != nil && len(raw) == 0 {
		return nil, unsupported("flate: %v", err)
	}
	if s.parms != nil {
		raw, err = applyPredictor(s.parms, raw)
		if err != nil {
			return nil, err
		}
	}
	if s.invert {
		for i := range raw {
			raw[i] = ^raw[i]
		}
	}
	return raw, nil
}
