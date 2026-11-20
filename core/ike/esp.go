package ike

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// IP protocol numbers carried in the ESP next-header field.
const (
	NextIPv4 = 4
	NextIPv6 = 41
	NextTCP  = 6
	NextUDP  = 17
)

// ESPSA is one direction of an ESP security association (RFC 4303). The
// same type serves the SWu tunnel and the IMS sec-agree transport SAs.
type ESPSA struct {
	SPI   uint32
	Suite Suite
	cs    *cipherState
	rnd   io.Reader

	mu     sync.Mutex
	seq    uint32 // outbound
	top    uint32 // inbound replay window
	bitmap uint64
}

// NewESPSA builds an SA from raw key material (encryption key incl. salt,
// then integrity key).
func NewESPSA(spi uint32, suite Suite, encKey, authKey []byte) (*ESPSA, error) {
	cs, err := newCipherState(suite.Enc, suite.Integ, encKey, authKey)
	if err != nil {
		return nil, err
	}
	return &ESPSA{SPI: spi, Suite: suite, cs: cs, rnd: rand.Reader}, nil
}

// Seal builds an ESP packet (SPI|Seq|IV|ct|ICV) around payload.
func (sa *ESPSA) Seal(payload []byte, nextHeader int) ([]byte, error) {
	sa.mu.Lock()
	sa.seq++
	seq := sa.seq
	sa.mu.Unlock()
	if seq == 0 {
		return nil, errors.New("esp: sequence number exhausted")
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, sa.SPI)
	binary.BigEndian.PutUint32(hdr[4:], seq)
	block := sa.Suite.Enc.block
	pt := padTrailer(payload, block, nextHeader)
	body, err := sa.cs.seal(sa.rnd, hdr, pt)
	if err != nil {
		return nil, err
	}
	return append(hdr, body...), nil
}

// Open authenticates and decrypts an ESP packet, returning the payload and
// next header.
func (sa *ESPSA) Open(pkt []byte) ([]byte, int, error) {
	if len(pkt) < 8 {
		return nil, 0, errors.New("esp: short packet")
	}
	seq := binary.BigEndian.Uint32(pkt[4:])
	if !sa.replayCheck(seq, false) {
		return nil, 0, fmt.Errorf("esp: replayed sequence %d", seq)
	}
	pt, err := sa.cs.open(pkt[:8], pkt[8:])
	if err != nil {
		return nil, 0, err
	}
	sa.replayCheck(seq, true)
	if len(pt) < 2 {
		return nil, 0, errors.New("esp: short plaintext")
	}
	nh := int(pt[len(pt)-1])
	padLen := int(pt[len(pt)-2])
	if padLen+2 > len(pt) {
		return nil, 0, errors.New("esp: bad padding")
	}
	return pt[:len(pt)-2-padLen], nh, nil
}

// replayCheck implements a 64-packet sliding window (RFC 4303 §3.4.3).
func (sa *ESPSA) replayCheck(seq uint32, update bool) bool {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	if seq == 0 {
		return false
	}
	if seq > sa.top {
		if update {
			shift := seq - sa.top
			if shift >= 64 {
				sa.bitmap = 0
			} else {
				sa.bitmap <<= shift
			}
			sa.bitmap |= 1
			sa.top = seq
		}
		return true
	}
	diff := sa.top - seq
	if diff >= 64 {
		return false
	}
	if sa.bitmap&(1<<diff) != 0 {
		return false
	}
	if update {
		sa.bitmap |= 1 << diff
	}
	return true
}

// ChildSA is a pair of ESP SAs negotiated through IKE.
type ChildSA struct {
	In  *ESPSA // ePDG -> UE, SPI chosen by us
	Out *ESPSA // UE -> ePDG, SPI chosen by the ePDG
}

// childKeys splits KEYMAT into the four keys of a child SA. weInitiated
// tells whether we were the initiator of the exchange that created it.
func childKeys(prf *prfAlg, skd, seed []byte, suite Suite, inSPI, outSPI uint32, weInitiated bool) (*ChildSA, error) {
	ek := suite.Enc.keyMaterial()
	ak := 0
	if suite.Integ != nil {
		ak = suite.Integ.keyLen
	}
	km := prf.prfPlus(skd, seed, 2*(ek+ak))
	iEnc, iAuth := km[:ek], km[ek:ek+ak]
	rEnc, rAuth := km[ek+ak:2*ek+ak], km[2*ek+ak:]
	outEnc, outAuth, inEnc, inAuth := iEnc, iAuth, rEnc, rAuth
	if !weInitiated {
		outEnc, outAuth, inEnc, inAuth = rEnc, rAuth, iEnc, iAuth
	}
	out, err := NewESPSA(outSPI, suite, outEnc, outAuth)
	if err != nil {
		return nil, err
	}
	in, err := NewESPSA(inSPI, suite, inEnc, inAuth)
	if err != nil {
		return nil, err
	}
	return &ChildSA{In: in, Out: out}, nil
}

// SuiteByName builds an ESP suite from algorithm names as used in this
// package ("aes128", "3des", "null"; "sha1", "md5", "sha256").
func SuiteByName(enc, integ string) (Suite, error) {
	e := encAlgs[enc]
	if e == nil {
		return Suite{}, fmt.Errorf("esp: unknown cipher %q", enc)
	}
	s := Suite{Enc: e}
	if integ != "" {
		if s.Integ = integAlgs[integ]; s.Integ == nil {
			return Suite{}, fmt.Errorf("esp: unknown integrity %q", integ)
		}
	}
	return s, nil
}
