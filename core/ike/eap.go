package ike

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"strings"
)

// EAP codes and types.
const (
	eapRequest  = 1
	eapResponse = 2
	eapSuccess  = 3
	eapFailure  = 4

	eapTypeIdentity = 1
	eapTypeNak      = 3
	eapTypeAKA      = 23
	eapTypeAKAPrime = 50
)

// EAP-AKA subtypes (RFC 4187 §11).
const (
	akaChallenge    = 1
	akaAuthReject   = 2
	akaSyncFailure  = 4
	akaIdentity     = 5
	akaNotification = 12
	akaReauth       = 13
	akaClientError  = 14
)

// EAP-AKA attributes.
const (
	atRAND            = 1
	atAUTN            = 2
	atRES             = 3
	atAUTS            = 4
	atPadding         = 6
	atPermanentIDReq  = 10
	atMAC             = 11
	atNotification    = 12
	atAnyIDReq        = 13
	atIdentity        = 14
	atFullauthIDReq   = 17
	atCounter         = 19
	atClientErrorCode = 22
	atKDFInput        = 23
	atKDF             = 24
	atResultInd       = 135
)

// AKAFunc runs UMTS AKA on the SIM. On synchronisation failure it returns
// auts != nil and err == nil.
type AKAFunc func(ctx context.Context, rand, autn []byte) (res, ck, ik, auts []byte, err error)

// ErrEAPFailure is returned when the server sends EAP-Failure.
var ErrEAPFailure = errors.New("ike: EAP authentication failed")

type eapAttr struct {
	typ   uint8
	value []byte // excludes type/length bytes
}

func parseEAPAttrs(b []byte) ([]eapAttr, error) {
	var out []eapAttr
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errors.New("eap: truncated attribute")
		}
		l := int(b[1]) * 4
		if l < 4 || l > len(b) {
			return nil, errors.New("eap: bad attribute length")
		}
		out = append(out, eapAttr{typ: b[0], value: b[2:l]})
		b = b[l:]
	}
	return out, nil
}

func encodeEAPAttr(typ uint8, value []byte) []byte {
	l := 2 + len(value)
	pad := (4 - l%4) % 4
	out := make([]byte, 0, l+pad)
	out = append(out, typ, byte((l+pad)/4))
	out = append(out, value...)
	return append(out, make([]byte, pad)...)
}

func findAttr(attrs []eapAttr, t uint8) *eapAttr {
	for i := range attrs {
		if attrs[i].typ == t {
			return &attrs[i]
		}
	}
	return nil
}

// eapAKA is the peer side of EAP-AKA (RFC 4187) and EAP-AKA' (RFC 9048).
type eapAKA struct {
	identity string
	aka      AKAFunc
	netName  string // AKA' access network name, set from AT_KDF_INPUT
	kAut     []byte
	msk      []byte
	prime    bool
	keysOK   bool
	// last names the previous step for diagnostics (no identities or keys).
	last string
}

func newEAPAKA(identity string, aka AKAFunc) *eapAKA {
	return &eapAKA{identity: identity, aka: aka}
}

// handle processes one EAP packet from the server. It returns the response
// to send (nil when none) and whether the conversation finished successfully.
func (e *eapAKA) handle(ctx context.Context, pkt []byte) (resp []byte, success bool, err error) {
	if len(pkt) < 4 {
		return nil, false, errors.New("eap: short packet")
	}
	code, id := pkt[0], pkt[1]
	l := int(binary.BigEndian.Uint16(pkt[2:]))
	if l > len(pkt) || l < 4 {
		return nil, false, errors.New("eap: bad length")
	}
	pkt = pkt[:l]
	switch code {
	case eapSuccess:
		if !e.keysOK {
			return nil, false, errors.New("eap: success before key derivation")
		}
		return nil, true, nil
	case eapFailure:
		if e.last == "" {
			return nil, false, ErrEAPFailure
		}
		return nil, false, fmt.Errorf("%w after %s", ErrEAPFailure, e.last)
	case eapRequest:
	default:
		return nil, false, fmt.Errorf("eap: unexpected code %d", code)
	}
	if len(pkt) < 5 {
		return nil, false, errors.New("eap: request without type")
	}
	typ := pkt[4]
	switch typ {
	case eapTypeIdentity:
		e.last = "EAP-Identity"
		return e.packet(eapResponse, id, eapTypeIdentity, []byte(e.identity)), false, nil
	case eapTypeAKA, eapTypeAKAPrime:
		e.prime = typ == eapTypeAKAPrime
	default:
		// Ask for EAP-AKA instead.
		e.last = fmt.Sprintf("EAP type %d (sent NAK)", typ)
		return e.packet(eapResponse, id, eapTypeNak, []byte{eapTypeAKA}), false, nil
	}
	if len(pkt) < 8 {
		return nil, false, errors.New("eap: short AKA packet")
	}
	subtype := pkt[5]
	attrs, err := parseEAPAttrs(pkt[8:])
	if err != nil {
		return nil, false, err
	}
	switch subtype {
	case akaIdentity:
		if findAttr(attrs, atPermanentIDReq) == nil && findAttr(attrs, atAnyIDReq) == nil && findAttr(attrs, atFullauthIDReq) == nil {
			return nil, false, errors.New("eap: identity request without ID attribute")
		}
		e.last = "AKA-Identity"
		if e.prime {
			e.last = "AKA'-Identity"
			// TS 23.003 §19.3.2 / RFC 9048: an EAP-AKA' permanent identity
			// starts with "6" (EAP-AKA uses "0"). Keys are derived from the
			// identity sent here, so keep it.
			if strings.HasPrefix(e.identity, "0") {
				e.identity = "6" + e.identity[1:]
			}
		}
		switch {
		case findAttr(attrs, atPermanentIDReq) != nil:
			e.last += " (permanent)"
		case findAttr(attrs, atFullauthIDReq) != nil:
			e.last += " (full-auth)"
		default:
			e.last += " (any)"
		}
		v := make([]byte, 2+len(e.identity))
		binary.BigEndian.PutUint16(v, uint16(len(e.identity)))
		copy(v[2:], e.identity)
		return e.akaPacket(id, akaIdentity, encodeEAPAttr(atIdentity, v), false), false, nil
	case akaChallenge:
		return e.challenge(ctx, id, pkt, attrs)
	case akaNotification:
		n := findAttr(attrs, atNotification)
		if n == nil || len(n.value) < 2 {
			return nil, false, errors.New("eap: notification without code")
		}
		codeN := binary.BigEndian.Uint16(n.value)
		e.last = fmt.Sprintf("AKA-Notification %d", codeN)
		withMAC := codeN&0x4000 == 0 // P bit clear: after challenge, MAC protected
		if withMAC && !e.verifyMAC(pkt, attrs) {
			return nil, false, errors.New("eap: notification MAC invalid")
		}
		r := e.akaPacket(id, akaNotification, nil, withMAC)
		if codeN&0x8000 == 0 {
			return r, false, fmt.Errorf("eap: server notification failure code %d", codeN&0x3fff)
		}
		return r, false, nil
	case akaReauth:
		e.last = "AKA-Reauthentication (sent client error)"
		v := []byte{0, 0} // unable to process packet
		return e.akaPacket(id, akaClientError, encodeEAPAttr(atClientErrorCode, v), false), false, nil
	default:
		return nil, false, fmt.Errorf("eap: unsupported AKA subtype %d", subtype)
	}
}

func (e *eapAKA) challenge(ctx context.Context, id byte, pkt []byte, attrs []eapAttr) ([]byte, bool, error) {
	ra, au := findAttr(attrs, atRAND), findAttr(attrs, atAUTN)
	if ra == nil || au == nil || len(ra.value) != 18 || len(au.value) != 18 {
		return nil, false, errors.New("eap: challenge without RAND/AUTN")
	}
	rand, autn := ra.value[2:], au.value[2:]
	if e.prime {
		kdf := findAttr(attrs, atKDF)
		in := findAttr(attrs, atKDFInput)
		if kdf == nil || in == nil || len(kdf.value) < 2 || binary.BigEndian.Uint16(kdf.value) != 1 {
			return nil, false, errors.New("eap: AKA' without supported KDF")
		}
		nl := int(binary.BigEndian.Uint16(in.value))
		if nl > len(in.value)-2 {
			return nil, false, errors.New("eap: bad AT_KDF_INPUT")
		}
		e.netName = string(in.value[2 : 2+nl])
	}
	e.last = "AKA-Challenge"
	if e.prime {
		e.last = "AKA'-Challenge"
	}
	res, ck, ik, auts, err := e.aka(ctx, rand, autn)
	if err != nil {
		// The SIM rejected AUTN (MAC failure): tell the server.
		return e.akaPacket(id, akaAuthReject, nil, false), false, fmt.Errorf("eap: SIM rejected challenge: %w", err)
	}
	if auts != nil {
		e.last += " (sent AUTS resync)"
		return e.akaPacket(id, akaSyncFailure, encodeEAPAttr(atAUTS, auts), false), false, nil
	}
	if e.prime {
		ck, ik = deriveCKIKPrime(ck, ik, autn[:6], e.netName)
		e.deriveKeysPrime(ik, ck)
	} else {
		e.deriveKeys(ik, ck)
	}
	if !e.verifyMAC(pkt, attrs) {
		return nil, false, errors.New("eap: challenge AT_MAC invalid")
	}
	v := make([]byte, 2+len(res))
	binary.BigEndian.PutUint16(v, uint16(len(res)*8))
	copy(v[2:], res)
	body := encodeEAPAttr(atRES, v)
	if findAttr(attrs, atResultInd) != nil {
		body = append(body, encodeEAPAttr(atResultInd, []byte{0, 0})...)
	}
	if e.prime {
		// Echo the KDF the server selected.
		body = append(body, encodeEAPAttr(atKDF, []byte{0, 1})...)
	}
	e.last += " (sent RES)"
	return e.akaPacket(id, akaChallenge, body, true), false, nil
}

func (e *eapAKA) packet(code, id, typ byte, data []byte) []byte {
	out := make([]byte, 5+len(data))
	out[0], out[1] = code, id
	binary.BigEndian.PutUint16(out[2:], uint16(len(out)))
	out[4] = typ
	copy(out[5:], data)
	return out
}

// akaPacket builds an EAP-AKA response, optionally appending AT_MAC.
func (e *eapAKA) akaPacket(id, subtype byte, attrs []byte, withMAC bool) []byte {
	typ := byte(eapTypeAKA)
	if e.prime {
		typ = eapTypeAKAPrime
	}
	body := append([]byte{subtype, 0, 0}, attrs...)
	macOff := -1
	if withMAC {
		macOff = 5 + len(body) + 4
		body = append(body, encodeEAPAttr(atMAC, make([]byte, 18))...)
	}
	pkt := e.packet(eapResponse, id, typ, body)
	if withMAC {
		copy(pkt[macOff:], e.mac(pkt))
	}
	return pkt
}

func (e *eapAKA) mac(pkt []byte) []byte {
	h := sha1.New
	if e.prime {
		h = sha256.New
	}
	m := hmac.New(h, e.kAut)
	m.Write(pkt)
	return m.Sum(nil)[:16]
}

// verifyMAC checks AT_MAC in pkt (computed with the MAC value zeroed).
func (e *eapAKA) verifyMAC(pkt []byte, attrs []eapAttr) bool {
	a := findAttr(attrs, atMAC)
	if a == nil || len(a.value) != 18 || e.kAut == nil {
		return false
	}
	off := -1
	for i := 8; i+20 <= len(pkt); {
		l := int(pkt[i+1]) * 4
		if pkt[i] == atMAC {
			off = i + 4
			break
		}
		if l == 0 {
			break
		}
		i += l
	}
	if off < 0 {
		return false
	}
	cp := append([]byte(nil), pkt...)
	got := append([]byte(nil), cp[off:off+16]...)
	for i := 0; i < 16; i++ {
		cp[off+i] = 0
	}
	return hmac.Equal(got, e.mac(cp))
}

// deriveKeys implements RFC 4187 §7: MK = SHA1(Identity|IK|CK) expanded with
// the FIPS 186-2 PRF into K_encr | K_aut | MSK | EMSK.
func (e *eapAKA) deriveKeys(ik, ck []byte) {
	h := sha1.New()
	h.Write([]byte(e.identity))
	h.Write(ik)
	h.Write(ck)
	mk := h.Sum(nil)
	x := fips186PRF(mk, 160)
	e.kAut = x[16:32]
	e.msk = x[32:96]
	e.keysOK = true
}

// deriveKeysPrime implements RFC 9048 §3.3 with PRF' = prf+ (HMAC-SHA256).
func (e *eapAKA) deriveKeysPrime(ikp, ckp []byte) {
	key := append(append([]byte(nil), ikp...), ckp...)
	seed := append([]byte("EAP-AKA'"), e.identity...)
	x := prfAlgs["sha256"].prfPlus(key, seed, 16+32+32+64+64)
	e.kAut = x[16:48]
	e.msk = x[80:144]
	e.keysOK = true
}

// deriveCKIKPrime derives CK'/IK' (TS 33.402 Annex A.2).
func deriveCKIKPrime(ck, ik, sqnXorAK []byte, netName string) ([]byte, []byte) {
	s := []byte{0x20}
	s = append(s, netName...)
	s = append(s, byte(len(netName)>>8), byte(len(netName)))
	s = append(s, sqnXorAK...)
	s = append(s, 0, 6)
	m := hmac.New(sha256.New, append(append([]byte(nil), ck...), ik...))
	m.Write(s)
	out := m.Sum(nil)
	return out[:16], out[16:]
}

// fips186PRF is the FIPS 186-2 (change notice 1) PRF from RFC 4187 App. A.
func fips186PRF(seed []byte, n int) []byte {
	var xkey [20]byte
	copy(xkey[:], seed)
	out := make([]byte, 0, n+40)
	for len(out) < n {
		for i := 0; i < 2; i++ {
			w := sha1G(xkey[:])
			out = append(out, w[:]...)
			// XKEY = (1 + XKEY + w) mod 2^160
			carry := uint32(1)
			for k := 19; k >= 0; k-- {
				s := uint32(xkey[k]) + uint32(w[k]) + carry
				xkey[k] = byte(s)
				carry = s >> 8
			}
		}
	}
	return out[:n]
}

// sha1G is the SHA-1 compression function applied once to the 160-bit input
// zero-padded to 512 bits, starting from the standard initial state.
func sha1G(x []byte) [20]byte {
	var block [64]byte
	copy(block[:], x)
	h := [5]uint32{0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476, 0xC3D2E1F0}
	var w [80]uint32
	for i := 0; i < 16; i++ {
		w[i] = binary.BigEndian.Uint32(block[i*4:])
	}
	for i := 16; i < 80; i++ {
		w[i] = bits.RotateLeft32(w[i-3]^w[i-8]^w[i-14]^w[i-16], 1)
	}
	a, b, c, d, e := h[0], h[1], h[2], h[3], h[4]
	for i := 0; i < 80; i++ {
		var f, k uint32
		switch {
		case i < 20:
			f, k = (b&c)|(^b&d), 0x5A827999
		case i < 40:
			f, k = b^c^d, 0x6ED9EBA1
		case i < 60:
			f, k = (b&c)|(b&d)|(c&d), 0x8F1BBCDC
		default:
			f, k = b^c^d, 0xCA62C1D6
		}
		t := bits.RotateLeft32(a, 5) + f + e + k + w[i]
		e, d, c, b, a = d, c, bits.RotateLeft32(b, 30), a, t
	}
	h[0] += a
	h[1] += b
	h[2] += c
	h[3] += d
	h[4] += e
	var out [20]byte
	for i := range h {
		binary.BigEndian.PutUint32(out[i*4:], h[i])
	}
	return out
}
