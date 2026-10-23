package pdfidx

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rc4"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// ErrPassword is returned for encrypted files when neither the empty password
// nor any supplied password opens them. It wraps ErrUnsupported.
var ErrPassword = fmt.Errorf("%w: password required or incorrect", ErrUnsupported)

type method int

const (
	mNone method = iota
	mRC4
	mAESV2
	mAESV3
)

var padding = []byte{
	0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41, 0x64, 0x00, 0x4E, 0x56, 0xFF, 0xFA, 0x01, 0x08,
	0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80, 0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A,
}

// crypt is the standard security handler state for one document.
type crypt struct {
	key []byte // file encryption key
	v   int
	stm method
}

type encParams struct {
	v, r      int
	keyLen    int // bytes
	o, u      []byte
	oe, ue    []byte
	p         uint32
	encryptMD bool
	id0       []byte
	stm       method
}

func (d *doc) setupCrypt(passwords [][]byte) error {
	ev, ok := d.trailer["Encrypt"]
	if !ok {
		return nil
	}
	ed, err := d.derefDict(ev)
	if err != nil {
		return err
	}
	if f := name2(ed["Filter"]); f != "Standard" {
		return unsupported("security handler %q", f)
	}
	ep := encParams{encryptMD: true}
	ep.v = int(intOf(ed, "V", 0))
	ep.r = int(intOf(ed, "R", 0))
	ep.p = uint32(intOf(ed, "P", 0))
	if b, ok := ed["EncryptMetadata"].(bool); ok {
		ep.encryptMD = b
	}
	ep.o, _ = ed["O"].(pstring)
	ep.u, _ = ed["U"].(pstring)
	ep.oe, _ = ed["OE"].(pstring)
	ep.ue, _ = ed["UE"].(pstring)
	if ids, ok := d.trailer["ID"].(array); ok && len(ids) > 0 {
		ep.id0, _ = ids[0].(pstring)
	}
	switch ep.v {
	case 1:
		ep.keyLen = 5
		ep.stm = mRC4
	case 2, 3:
		ep.keyLen = int(intOf(ed, "Length", 40)) / 8
		ep.stm = mRC4
	case 4, 5:
		ep.stm = mNone
		cf, _ := d.derefDict(ed["CF"])
		sn := name2(ed["StmF"])
		if sn == "" {
			sn = "Identity"
		}
		if sn != "Identity" {
			f, err := d.derefDict(cf[sn])
			if err != nil {
				return unsupported("crypt filter %q", sn)
			}
			switch name2(f["CFM"]) {
			case "V2":
				ep.stm = mRC4
			case "AESV2":
				ep.stm = mAESV2
			case "AESV3":
				ep.stm = mAESV3
			case "None":
				ep.stm = mNone
			default:
				return unsupported("crypt filter method %q", name2(f["CFM"]))
			}
		}
		if ep.v == 4 {
			ep.keyLen = 16
		} else {
			ep.keyLen = 32
		}
	default:
		return unsupported("encryption V=%d", ep.v)
	}
	if ep.keyLen < 5 || ep.keyLen > 32 {
		return unsupported("bad key length %d", ep.keyLen)
	}
	if ep.r < 2 || ep.r > 6 {
		return unsupported("encryption R=%d", ep.r)
	}

	cands := [][]byte{nil}
	for _, pw := range passwords {
		cands = append(cands, pw)
		if l1, ok := latin1(pw); ok && !bytes.Equal(l1, pw) {
			cands = append(cands, l1)
		}
	}
	for _, pw := range cands {
		if key := ep.tryPassword(pw); key != nil {
			d.crypt = &crypt{key: key, v: ep.v, stm: ep.stm}
			return nil
		}
	}
	return ErrPassword
}

func latin1(b []byte) ([]byte, bool) {
	if utf8.Valid(b) {
		out := make([]byte, 0, len(b))
		for _, r := range string(b) {
			if r > 255 {
				return nil, false
			}
			out = append(out, byte(r))
		}
		return out, true
	}
	return nil, false
}

func (ep *encParams) tryPassword(pw []byte) []byte {
	if ep.r >= 5 {
		return ep.tryPasswordR56(pw)
	}
	if k := ep.userKey(pw); k != nil {
		return k
	}
	// pw as owner password: recover the user password from /O.
	if up := ep.userFromOwner(pw); up != nil {
		return ep.userKey(up)
	}
	return nil
}

func pad32(pw []byte) []byte {
	if len(pw) > 32 {
		pw = pw[:32]
	}
	return append(append([]byte{}, pw...), padding[:32-len(pw)]...)
}

func (ep *encParams) userKey(pw []byte) []byte {
	h := md5.New()
	h.Write(pad32(pw))
	h.Write(ep.o)
	var pb [4]byte
	binary.LittleEndian.PutUint32(pb[:], ep.p)
	h.Write(pb[:])
	h.Write(ep.id0)
	if ep.r >= 4 && !ep.encryptMD {
		h.Write([]byte{0xff, 0xff, 0xff, 0xff})
	}
	sum := h.Sum(nil)
	n := ep.keyLen
	if ep.r >= 3 {
		for i := 0; i < 50; i++ {
			s := md5.Sum(sum[:n])
			sum = s[:]
		}
	} else {
		n = 5
	}
	key := append([]byte{}, sum[:n]...)

	if ep.r == 2 {
		c, err := rc4.NewCipher(key)
		if err != nil {
			return nil
		}
		out := make([]byte, 32)
		c.XORKeyStream(out, padding)
		if bytes.Equal(out, ep.u) {
			return key
		}
		return nil
	}
	h2 := md5.New()
	h2.Write(padding)
	h2.Write(ep.id0)
	x := h2.Sum(nil)
	for i := 0; i < 20; i++ {
		k := make([]byte, len(key))
		for j := range key {
			k[j] = key[j] ^ byte(i)
		}
		c, err := rc4.NewCipher(k)
		if err != nil {
			return nil
		}
		c.XORKeyStream(x, x)
	}
	if len(ep.u) >= 16 && bytes.Equal(x[:16], ep.u[:16]) {
		return key
	}
	return nil
}

func (ep *encParams) userFromOwner(pw []byte) []byte {
	s := md5.Sum(pad32(pw))
	sum := s[:]
	n := 5
	if ep.r >= 3 {
		n = ep.keyLen
		for i := 0; i < 50; i++ {
			s := md5.Sum(sum)
			sum = s[:]
		}
	}
	key := sum[:n]
	out := append([]byte{}, ep.o...)
	if len(out) < 32 {
		return nil
	}
	if ep.r == 2 {
		c, err := rc4.NewCipher(key)
		if err != nil {
			return nil
		}
		c.XORKeyStream(out, out)
		return out[:32]
	}
	for i := 19; i >= 0; i-- {
		k := make([]byte, len(key))
		for j := range key {
			k[j] = key[j] ^ byte(i)
		}
		c, err := rc4.NewCipher(k)
		if err != nil {
			return nil
		}
		c.XORKeyStream(out, out)
	}
	return out[:32]
}

func (ep *encParams) tryPasswordR56(pw []byte) []byte {
	if len(pw) > 127 {
		pw = pw[:127]
	}
	if len(ep.u) < 48 || len(ep.o) < 48 || len(ep.ue) < 32 {
		return nil
	}
	hash := hashR5
	if ep.r == 6 {
		hash = hashR6
	}
	if bytes.Equal(hash(pw, ep.u[32:40], nil), ep.u[:32]) {
		return aes256NoPad(hash(pw, ep.u[40:48], nil), ep.ue[:32])
	}
	if len(ep.oe) >= 32 && bytes.Equal(hash(pw, ep.o[32:40], ep.u[:48]), ep.o[:32]) {
		return aes256NoPad(hash(pw, ep.o[40:48], ep.u[:48]), ep.oe[:32])
	}
	return nil
}

func aes256NoPad(key, data []byte) []byte {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(b, make([]byte, 16)).CryptBlocks(out, data)
	return out
}

func hashR5(pw, salt, udata []byte) []byte {
	h := sha256.New()
	h.Write(pw)
	h.Write(salt)
	h.Write(udata)
	return h.Sum(nil)
}

// hashR6 is ISO 32000-2 algorithm 2.B.
func hashR6(pw, salt, udata []byte) []byte {
	k := hashR5(pw, salt, udata)
	for i := 0; ; {
		k1 := bytes.Repeat(append(append(append([]byte{}, pw...), k...), udata...), 64)
		blk, err := aes.NewCipher(k[:16])
		if err != nil {
			return nil
		}
		e := make([]byte, len(k1))
		cipher.NewCBCEncrypter(blk, k[16:32]).CryptBlocks(e, k1)
		mod := 0
		for _, b := range e[:16] {
			mod += int(b)
		}
		switch mod % 3 {
		case 0:
			s := sha256.Sum256(e)
			k = s[:]
		case 1:
			s := sha512.Sum384(e)
			k = s[:]
		default:
			s := sha512.Sum512(e)
			k = s[:]
		}
		i++
		if i >= 64 && int(e[len(e)-1]) <= i-32 {
			break
		}
	}
	return k[:32]
}

func (c *crypt) objKey(num, gen int, aes bool) []byte {
	if c.v >= 5 {
		return c.key
	}
	h := md5.New()
	h.Write(c.key)
	h.Write([]byte{byte(num), byte(num >> 8), byte(num >> 16), byte(gen), byte(gen >> 8)})
	if aes {
		h.Write([]byte("sAlT"))
	}
	n := len(c.key) + 5
	if n > 16 {
		n = 16
	}
	return h.Sum(nil)[:n]
}

func (c *crypt) identity() bool { return c.stm == mNone }

func (c *crypt) decryptStream(num, gen int, raw []byte) ([]byte, error) {
	switch c.stm {
	case mNone:
		return raw, nil
	case mRC4:
		ci, err := rc4.NewCipher(c.objKey(num, gen, false))
		if err != nil {
			return nil, unsupported("rc4: %v", err)
		}
		out := make([]byte, len(raw))
		ci.XORKeyStream(out, raw)
		return out, nil
	default:
		return aesCBCDecrypt(c.objKey(num, gen, true), raw)
	}
}

func aesCBCDecrypt(key, raw []byte) ([]byte, error) {
	if len(raw) < 32 || len(raw)%16 != 0 {
		return nil, unsupported("bad AES stream length %d", len(raw))
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, unsupported("aes: %v", err)
	}
	out := make([]byte, len(raw)-16)
	cipher.NewCBCDecrypter(b, raw[:16]).CryptBlocks(out, raw[16:])
	pad := int(out[len(out)-1])
	if pad < 1 || pad > 16 || pad > len(out) {
		return nil, unsupported("bad AES padding")
	}
	return out[:len(out)-pad], nil
}

// plainSize returns the decrypted length of an image stream of ciphertext
// length n at off, reading at most the last 32 bytes.
func (c *crypt) plainSize(r io.ReaderAt, num, gen int, off, n int64) (int64, error) {
	switch c.stm {
	case mNone, mRC4:
		return n, nil
	}
	if n < 32 || n%16 != 0 {
		return 0, unsupported("bad AES stream length %d", n)
	}
	tail := make([]byte, 32)
	if _, err := r.ReadAt(tail, off+n-32); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	b, err := aes.NewCipher(c.objKey(num, gen, true))
	if err != nil {
		return 0, unsupported("aes: %v", err)
	}
	last := make([]byte, 16)
	cipher.NewCBCDecrypter(b, tail[:16]).CryptBlocks(last, tail[16:])
	pad := int64(last[15])
	if pad < 1 || pad > 16 || n-16-pad < 0 {
		return 0, unsupported("bad AES padding")
	}
	return n - 16 - pad, nil
}
