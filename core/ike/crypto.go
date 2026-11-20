package ike

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"strings"
)

// Transform types.
const (
	TransformENCR  = 1
	TransformPRF   = 2
	TransformINTEG = 3
	TransformDH    = 4
	TransformESN   = 5
)

// Protocol IDs.
const (
	ProtoIKE = 1
	ProtoESP = 3
)

// ---------------------------------------------------------------- algorithms

type encAlg struct {
	name    string
	id      uint16
	keyLen  int // bytes, excluding salt
	keyAttr bool
	aead    bool
	block   int
	ivLen   int
	saltLen int
	icvLen  int // AEAD tag length
}

func (e *encAlg) keyMaterial() int { return e.keyLen + e.saltLen }

var encAlgs = map[string]*encAlg{
	"aes128":    {name: "aes128", id: 12, keyLen: 16, keyAttr: true, block: 16, ivLen: 16},
	"aes192":    {name: "aes192", id: 12, keyLen: 24, keyAttr: true, block: 16, ivLen: 16},
	"aes256":    {name: "aes256", id: 12, keyLen: 32, keyAttr: true, block: 16, ivLen: 16},
	"aes128gcm": {name: "aes128gcm", id: 20, keyLen: 16, keyAttr: true, aead: true, block: 4, ivLen: 8, saltLen: 4, icvLen: 16},
	"aes256gcm": {name: "aes256gcm", id: 20, keyLen: 32, keyAttr: true, aead: true, block: 4, ivLen: 8, saltLen: 4, icvLen: 16},
	"3des":      {name: "3des", id: 3, keyLen: 24, block: 8, ivLen: 8},
	"null":      {name: "null", id: 11, keyLen: 0, block: 4, ivLen: 0},
}

type integAlg struct {
	name   string
	id     uint16
	keyLen int
	outLen int
	h      func() hash.Hash
}

var integAlgs = map[string]*integAlg{
	"md5":    {name: "md5", id: 1, keyLen: 16, outLen: 12, h: md5.New},
	"sha1":   {name: "sha1", id: 2, keyLen: 20, outLen: 12, h: sha1.New},
	"sha256": {name: "sha256", id: 12, keyLen: 32, outLen: 16, h: sha256.New},
	"sha384": {name: "sha384", id: 13, keyLen: 48, outLen: 24, h: sha512.New384},
	"sha512": {name: "sha512", id: 14, keyLen: 64, outLen: 32, h: sha512.New},
}

type prfAlg struct {
	name string
	id   uint16
	h    func() hash.Hash
	size int
}

var prfAlgs = map[string]*prfAlg{
	"md5":    {name: "md5", id: 1, h: md5.New, size: 16},
	"sha1":   {name: "sha1", id: 2, h: sha1.New, size: 20},
	"sha256": {name: "sha256", id: 5, h: sha256.New, size: 32},
	"sha384": {name: "sha384", id: 6, h: sha512.New384, size: 48},
	"sha512": {name: "sha512", id: 7, h: sha512.New, size: 64},
}

func (p *prfAlg) prf(key, data []byte) []byte {
	m := hmac.New(p.h, key)
	m.Write(data)
	return m.Sum(nil)
}

// prfPlus implements prf+ (RFC 7296 §2.13).
func (p *prfAlg) prfPlus(key, seed []byte, n int) []byte {
	out := make([]byte, 0, n+p.size)
	var t []byte
	for i := 1; len(out) < n; i++ {
		buf := append(append(append([]byte(nil), t...), seed...), byte(i))
		t = p.prf(key, buf)
		out = append(out, t...)
		if i == 255 {
			break
		}
	}
	return out[:n]
}

// ------------------------------------------------------------------ DH groups

type dhGroup interface {
	ID() uint16
	Public() []byte
	Shared(peer []byte) ([]byte, error)
}

const (
	modp1024 = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381FFFFFFFFFFFFFFFF"
	modp1536 = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA237327FFFFFFFFFFFFFFFF"
	modp2048 = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AACAA68FFFFFFFFFFFFFFFF"
)

type modpGroup struct {
	id   uint16
	p    *big.Int
	priv *big.Int
	pub  []byte
}

func newMODP(id uint16, hexP string, rnd io.Reader) (*modpGroup, error) {
	p, _ := new(big.Int).SetString(hexP, 16)
	size := (p.BitLen() + 7) / 8
	k := make([]byte, 32) // 256-bit exponent is ample for these groups
	if _, err := io.ReadFull(rnd, k); err != nil {
		return nil, err
	}
	priv := new(big.Int).SetBytes(k)
	pub := new(big.Int).Exp(big.NewInt(2), priv, p)
	return &modpGroup{id: id, p: p, priv: priv, pub: pub.FillBytes(make([]byte, size))}, nil
}

func (g *modpGroup) ID() uint16     { return g.id }
func (g *modpGroup) Public() []byte { return g.pub }
func (g *modpGroup) Shared(peer []byte) ([]byte, error) {
	size := (g.p.BitLen() + 7) / 8
	if len(peer) != size {
		return nil, fmt.Errorf("ike: DH group %d public value has %d bytes", g.id, len(peer))
	}
	y := new(big.Int).SetBytes(peer)
	if y.Cmp(big.NewInt(1)) <= 0 || y.Cmp(new(big.Int).Sub(g.p, big.NewInt(1))) >= 0 {
		return nil, errors.New("ike: invalid DH public value")
	}
	return new(big.Int).Exp(y, g.priv, g.p).FillBytes(make([]byte, size)), nil
}

type ecGroup struct {
	id    uint16
	curve ecdh.Curve
	priv  *ecdh.PrivateKey
	raw   bool // curve25519: no 0x04 prefix stripping
}

func (g *ecGroup) ID() uint16 { return g.id }
func (g *ecGroup) Public() []byte {
	b := g.priv.PublicKey().Bytes()
	if g.raw {
		return b
	}
	return b[1:] // RFC 5903: x || y without the uncompressed point marker
}
func (g *ecGroup) Shared(peer []byte) ([]byte, error) {
	if !g.raw {
		peer = append([]byte{4}, peer...)
	}
	pk, err := g.curve.NewPublicKey(peer)
	if err != nil {
		return nil, err
	}
	return g.priv.ECDH(pk)
}

func newDH(id uint16, rnd io.Reader) (dhGroup, error) {
	switch id {
	case 2:
		return newMODP(2, modp1024, rnd)
	case 5:
		return newMODP(5, modp1536, rnd)
	case 14:
		return newMODP(14, modp2048, rnd)
	case 19, 20, 31:
		var c ecdh.Curve
		switch id {
		case 19:
			c = ecdh.P256()
		case 20:
			c = ecdh.P384()
		default:
			c = ecdh.X25519()
		}
		k, err := c.GenerateKey(rnd)
		if err != nil {
			return nil, err
		}
		return &ecGroup{id: id, curve: c, priv: k, raw: id == 31}, nil
	}
	return nil, fmt.Errorf("ike: unsupported DH group %d", id)
}

// ------------------------------------------------------------------ proposals

// Transform is one SA transform.
type Transform struct {
	Type   uint8
	ID     uint16
	KeyLen uint16 // bits; 0 = no key length attribute
}

// Proposal is one SA proposal.
type Proposal struct {
	Num        uint8
	Protocol   uint8
	SPI        []byte
	Transforms []Transform
}

func encodeSA(props []Proposal) []byte {
	var out []byte
	for i, p := range props {
		var tb []byte
		for j, t := range p.Transforms {
			last := byte(3)
			if j == len(p.Transforms)-1 {
				last = 0
			}
			l := 8
			if t.KeyLen != 0 {
				l = 12
			}
			e := []byte{last, 0, 0, byte(l), t.Type, 0, byte(t.ID >> 8), byte(t.ID)}
			if t.KeyLen != 0 {
				e = append(e, 0x80, 0x0E, byte(t.KeyLen>>8), byte(t.KeyLen))
			}
			tb = append(tb, e...)
		}
		last := byte(2)
		if i == len(props)-1 {
			last = 0
		}
		num := p.Num
		if num == 0 {
			num = uint8(i + 1)
		}
		l := 8 + len(p.SPI) + len(tb)
		h := []byte{last, 0, byte(l >> 8), byte(l), num, p.Protocol, byte(len(p.SPI)), byte(len(p.Transforms))}
		out = append(out, h...)
		out = append(out, p.SPI...)
		out = append(out, tb...)
	}
	return out
}

func decodeSA(b []byte) ([]Proposal, error) {
	var out []Proposal
	for len(b) > 0 {
		if len(b) < 8 {
			return nil, errors.New("ike: short proposal")
		}
		l := int(binary.BigEndian.Uint16(b[2:]))
		if l < 8 || l > len(b) {
			return nil, errors.New("ike: bad proposal length")
		}
		p := Proposal{Num: b[4], Protocol: b[5]}
		spiLen, nt := int(b[6]), int(b[7])
		body := b[8:l]
		if len(body) < spiLen {
			return nil, errors.New("ike: bad proposal SPI")
		}
		p.SPI = body[:spiLen]
		body = body[spiLen:]
		for i := 0; i < nt; i++ {
			if len(body) < 8 {
				return nil, errors.New("ike: short transform")
			}
			tl := int(binary.BigEndian.Uint16(body[2:]))
			if tl < 8 || tl > len(body) {
				return nil, errors.New("ike: bad transform length")
			}
			t := Transform{Type: body[4], ID: binary.BigEndian.Uint16(body[6:])}
			attrs := body[8:tl]
			for len(attrs) >= 4 {
				at := binary.BigEndian.Uint16(attrs)
				if at&0x8000 != 0 {
					if at&0x7fff == 14 {
						t.KeyLen = binary.BigEndian.Uint16(attrs[2:])
					}
					attrs = attrs[4:]
				} else {
					al := int(binary.BigEndian.Uint16(attrs[2:]))
					if 4+al > len(attrs) {
						break
					}
					attrs = attrs[4+al:]
				}
			}
			p.Transforms = append(p.Transforms, t)
			body = body[tl:]
		}
		out = append(out, p)
		if b[0] == 0 {
			break
		}
		b = b[l:]
	}
	return out, nil
}

// Suite is a negotiated algorithm set.
type Suite struct {
	Enc   *encAlg
	Integ *integAlg // nil for AEAD
	PRF   *prfAlg   // IKE only
	DH    uint16    // IKE only
	ESN   bool
}

func (s Suite) String() string {
	parts := []string{s.Enc.name}
	if s.Integ != nil {
		parts = append(parts, s.Integ.name)
	}
	if s.PRF != nil {
		parts = append(parts, "prf"+s.PRF.name)
	}
	if s.DH != 0 {
		parts = append(parts, fmt.Sprintf("dh%d", s.DH))
	}
	return strings.Join(parts, "-")
}

// ProposalConfig lists acceptable algorithms by name, most preferred first.
// The first DH group is used for the initial KE payload.
type ProposalConfig struct {
	IKEEnc   []string `json:"ike_enc,omitempty"`
	IKEInteg []string `json:"ike_integ,omitempty"`
	IKEPRF   []string `json:"ike_prf,omitempty"`
	IKEDH    []uint16 `json:"ike_dh,omitempty"`
	ESPEnc   []string `json:"esp_enc,omitempty"`
	ESPInteg []string `json:"esp_integ,omitempty"`
}

// DefaultProposals is broad enough for the ePDGs of most operators.
func DefaultProposals() ProposalConfig {
	return ProposalConfig{
		IKEEnc:   []string{"aes256", "aes128", "aes256gcm", "aes128gcm", "3des"},
		IKEInteg: []string{"sha256", "sha1", "sha384", "sha512", "md5"},
		IKEPRF:   []string{"sha256", "sha1", "sha384", "sha512", "md5"},
		IKEDH:    []uint16{14, 2, 5, 19, 20},
		ESPEnc:   []string{"aes128", "aes256", "aes128gcm", "aes256gcm", "3des", "null"},
		ESPInteg: []string{"sha1", "sha256", "sha384", "sha512", "md5"},
	}
}

func (c ProposalConfig) withDefaults() ProposalConfig {
	d := DefaultProposals()
	if len(c.IKEEnc) == 0 {
		c.IKEEnc = d.IKEEnc
	}
	if len(c.IKEInteg) == 0 {
		c.IKEInteg = d.IKEInteg
	}
	if len(c.IKEPRF) == 0 {
		c.IKEPRF = d.IKEPRF
	}
	if len(c.IKEDH) == 0 {
		c.IKEDH = d.IKEDH
	}
	if len(c.ESPEnc) == 0 {
		c.ESPEnc = d.ESPEnc
	}
	if len(c.ESPInteg) == 0 {
		c.ESPInteg = d.ESPInteg
	}
	return c
}

func encTransform(e *encAlg) Transform {
	t := Transform{Type: TransformENCR, ID: e.id}
	if e.keyAttr {
		t.KeyLen = uint16(e.keyLen * 8)
	}
	return t
}

// buildProposals splits AEAD and non-AEAD ciphers into separate proposals.
func buildProposals(proto uint8, spi []byte, c ProposalConfig, dhFirst uint16) []Proposal {
	var normal, aead []Transform
	encs, integs := c.ESPEnc, c.ESPInteg
	if proto == ProtoIKE {
		encs, integs = c.IKEEnc, c.IKEInteg
	}
	for _, n := range encs {
		e := encAlgs[n]
		if e == nil || (proto == ProtoIKE && e.name == "null") {
			continue
		}
		if e.aead {
			aead = append(aead, encTransform(e))
		} else {
			normal = append(normal, encTransform(e))
		}
	}
	var tail []Transform
	if proto == ProtoIKE {
		for _, n := range c.IKEPRF {
			if p := prfAlgs[n]; p != nil {
				tail = append(tail, Transform{Type: TransformPRF, ID: p.id})
			}
		}
		groups := []uint16{dhFirst}
		for _, g := range c.IKEDH {
			if g != dhFirst {
				groups = append(groups, g)
			}
		}
		for _, g := range groups {
			tail = append(tail, Transform{Type: TransformDH, ID: g})
		}
	} else {
		tail = append(tail, Transform{Type: TransformESN, ID: 0})
	}
	var integT []Transform
	for _, n := range integs {
		if a := integAlgs[n]; a != nil {
			integT = append(integT, Transform{Type: TransformINTEG, ID: a.id})
		}
	}
	var props []Proposal
	if len(normal) > 0 {
		ts := append(append(append([]Transform{}, normal...), integT...), tail...)
		props = append(props, Proposal{Protocol: proto, SPI: spi, Transforms: ts})
	}
	if len(aead) > 0 {
		ts := append(append([]Transform{}, aead...), tail...)
		props = append(props, Proposal{Protocol: proto, SPI: spi, Transforms: ts})
	}
	for i := range props {
		props[i].Num = uint8(i + 1)
	}
	return props
}

// suiteFromProposal maps a responder-selected proposal to algorithms.
func suiteFromProposal(p Proposal) (Suite, error) {
	var s Suite
	for _, t := range p.Transforms {
		switch t.Type {
		case TransformENCR:
			for _, e := range encAlgs {
				if e.id == t.ID && (!e.keyAttr || uint16(e.keyLen*8) == t.KeyLen) {
					s.Enc = e
				}
			}
			if s.Enc == nil {
				return s, fmt.Errorf("ike: unsupported cipher %d/%d", t.ID, t.KeyLen)
			}
		case TransformINTEG:
			for _, a := range integAlgs {
				if a.id == t.ID {
					s.Integ = a
				}
			}
			if s.Integ == nil && t.ID != 0 {
				return s, fmt.Errorf("ike: unsupported integrity %d", t.ID)
			}
		case TransformPRF:
			for _, a := range prfAlgs {
				if a.id == t.ID {
					s.PRF = a
				}
			}
			if s.PRF == nil {
				return s, fmt.Errorf("ike: unsupported PRF %d", t.ID)
			}
		case TransformDH:
			s.DH = t.ID
		case TransformESN:
			s.ESN = t.ID == 1
		}
	}
	if s.Enc == nil {
		return s, errors.New("ike: proposal without cipher")
	}
	if !s.Enc.aead && s.Integ == nil {
		return s, errors.New("ike: proposal without integrity")
	}
	return s, nil
}

// ------------------------------------------------------------ SK payload codec

// cipherState holds one direction of an encrypted channel (IKE SK or ESP).
type cipherState struct {
	enc     *encAlg
	integ   *integAlg
	encKey  []byte
	salt    []byte
	authKey []byte
	block   cipher.Block
	aead    cipher.AEAD
}

func newCipherState(enc *encAlg, integ *integAlg, encKeyMat, authKey []byte) (*cipherState, error) {
	cs := &cipherState{enc: enc, integ: integ, authKey: authKey}
	key := encKeyMat[:enc.keyLen]
	cs.salt = encKeyMat[enc.keyLen:]
	cs.encKey = key
	var err error
	switch {
	case enc.name == "null":
	case enc.name == "3des":
		cs.block, err = des.NewTripleDESCipher(key)
	default:
		cs.block, err = aes.NewCipher(key)
		if err == nil && enc.aead {
			cs.aead, err = cipher.NewGCMWithTagSize(cs.block, enc.icvLen)
		}
	}
	return cs, err
}

func (cs *cipherState) icvLen() int {
	if cs.enc.aead {
		return cs.enc.icvLen
	}
	if cs.integ == nil {
		return 0
	}
	return cs.integ.outLen
}

func (cs *cipherState) mac(data []byte) []byte {
	m := hmac.New(cs.integ.h, cs.authKey)
	m.Write(data)
	return m.Sum(nil)[:cs.integ.outLen]
}

// seal encrypts plaintext (already padded with pad length / next header
// trailer by the caller) and returns IV || ciphertext || ICV, where aad is
// everything that precedes the IV in the packet. For non-AEAD ciphers the ICV
// covers aad || IV || ciphertext.
func (cs *cipherState) seal(rnd io.Reader, aad, plain []byte) ([]byte, error) {
	iv := make([]byte, cs.enc.ivLen)
	if _, err := io.ReadFull(rnd, iv); err != nil {
		return nil, err
	}
	if cs.enc.aead {
		nonce := append(append([]byte(nil), cs.salt...), iv...)
		ct := cs.aead.Seal(nil, nonce, plain, aad)
		return append(iv, ct...), nil
	}
	ct := make([]byte, len(plain))
	switch {
	case cs.block == nil:
		copy(ct, plain)
	default:
		if len(plain)%cs.block.BlockSize() != 0 {
			return nil, errors.New("ike: plaintext not block aligned")
		}
		cipher.NewCBCEncrypter(cs.block, iv).CryptBlocks(ct, plain)
	}
	out := append(iv, ct...)
	if cs.integ != nil {
		out = append(out, cs.mac(append(append([]byte(nil), aad...), out...))...)
	}
	return out, nil
}

// open reverses seal. body is IV || ciphertext || ICV.
func (cs *cipherState) open(aad, body []byte) ([]byte, error) {
	icv := cs.icvLen()
	if len(body) < cs.enc.ivLen+icv {
		return nil, errors.New("ike: encrypted body too short")
	}
	iv := body[:cs.enc.ivLen]
	if cs.enc.aead {
		nonce := append(append([]byte(nil), cs.salt...), iv...)
		return cs.aead.Open(nil, nonce, body[cs.enc.ivLen:], aad)
	}
	ct := body[cs.enc.ivLen : len(body)-icv]
	if cs.integ != nil {
		want := cs.mac(append(append([]byte(nil), aad...), body[:len(body)-icv]...))
		if !hmac.Equal(want, body[len(body)-icv:]) {
			return nil, errors.New("ike: integrity check failed")
		}
	}
	if cs.block == nil {
		return append([]byte(nil), ct...), nil
	}
	if len(ct)%cs.block.BlockSize() != 0 {
		return nil, errors.New("ike: ciphertext not block aligned")
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(cs.block, iv).CryptBlocks(pt, ct)
	return pt, nil
}

// padTrailer pads data for the cipher block size, appending the pad length
// byte (and nextHeader if >= 0, for ESP).
func padTrailer(data []byte, block int, nextHeader int) []byte {
	trailer := 1
	if nextHeader >= 0 {
		trailer = 2
	}
	if block < 4 {
		block = 4
	}
	pad := (block - (len(data)+trailer)%block) % block
	out := append([]byte(nil), data...)
	for i := 1; i <= pad; i++ {
		out = append(out, byte(i))
	}
	out = append(out, byte(pad))
	if nextHeader >= 0 {
		out = append(out, byte(nextHeader))
	}
	return out
}
