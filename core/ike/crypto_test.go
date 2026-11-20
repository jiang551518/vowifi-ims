package ike

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 4186 Appendix A: the FIPS 186-2 PRF output for a known MK.
func TestFIPS186PRF(t *testing.T) {
	mk := unhex(t, "e576d5ca 332e9930 018bf1ba ee2763c7 95b3c712")
	x := fips186PRF(mk, 160)
	if want := unhex(t, "536e5ebc 4465582a a6a8ec99 86ebb620"); !bytes.Equal(x[:16], want) {
		t.Fatalf("K_encr = %x, want %x", x[:16], want)
	}
	if want := unhex(t, "25af1942 efcbf4bc 72b39434 21f2a974"); !bytes.Equal(x[16:32], want) {
		t.Fatalf("K_aut = %x, want %x", x[16:32], want)
	}
}

func TestMODPPrimesAreSafePrimes(t *testing.T) {
	for _, h := range []string{modp1024, modp1536, modp2048} {
		p, ok := new(big.Int).SetString(h, 16)
		if !ok {
			t.Fatal("bad hex")
		}
		q := new(big.Int).Rsh(p, 1)
		if !p.ProbablyPrime(20) || !q.ProbablyPrime(20) {
			t.Fatalf("%d-bit MODP prime is not a safe prime (typo?)", p.BitLen())
		}
	}
}

func TestDHAgreement(t *testing.T) {
	for _, g := range []uint16{2, 5, 14, 19, 20, 31} {
		a, err := newDH(g, nil2rand())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := newDH(g, nil2rand())
		s1, err1 := a.Shared(b.Public())
		s2, err2 := b.Shared(a.Public())
		if err1 != nil || err2 != nil || !bytes.Equal(s1, s2) {
			t.Fatalf("group %d: shared secrets differ (%v %v)", g, err1, err2)
		}
	}
}

func TestSAProposalRoundTrip(t *testing.T) {
	props := buildProposals(ProtoIKE, nil, DefaultProposals(), 14)
	dec, err := decodeSA(encodeSA(props))
	if err != nil || len(dec) != len(props) {
		t.Fatalf("decode: %v (%d vs %d)", err, len(dec), len(props))
	}
	for i := range props {
		if len(dec[i].Transforms) != len(props[i].Transforms) {
			t.Fatalf("proposal %d transform count", i)
		}
	}
	s, err := suiteFromProposal(Proposal{Protocol: ProtoIKE, Transforms: []Transform{
		{Type: TransformENCR, ID: 12, KeyLen: 256}, {Type: TransformINTEG, ID: 12},
		{Type: TransformPRF, ID: 5}, {Type: TransformDH, ID: 14}}})
	if err != nil || s.String() != "aes256-sha256-prfsha256-dh14" {
		t.Fatalf("suite %v %v", s, err)
	}
}

func TestCipherRoundTrip(t *testing.T) {
	for _, n := range []string{"aes128", "aes256", "aes128gcm", "3des"} {
		e := encAlgs[n]
		var integ *integAlg
		if !e.aead {
			integ = integAlgs["sha1"]
		}
		key := bytes.Repeat([]byte{7}, e.keyMaterial())
		cs, err := newCipherState(e, integ, key, bytes.Repeat([]byte{9}, 20))
		if err != nil {
			t.Fatal(err)
		}
		aad := []byte("header")
		pt := padTrailer([]byte("hello ike world"), e.block, -1)
		ct, err := cs.seal(nil2rand(), aad, pt)
		if err != nil {
			t.Fatal(err)
		}
		out, err := cs.open(aad, ct)
		if err != nil || !bytes.Equal(out, pt) {
			t.Fatalf("%s: round trip failed: %v", n, err)
		}
		ct[len(ct)-1] ^= 1
		if _, err := cs.open(aad, ct); err == nil {
			t.Fatalf("%s: tampering not detected", n)
		}
	}
}
