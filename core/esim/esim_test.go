package esim

import "testing"

func TestICCIDRoundTrip(t *testing.T) {
	for _, s := range []string{"8944000000000000001", "89860000000000000012"} {
		b, err := encodeICCID(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeICCID(b); got != s {
			t.Fatalf("%s -> %s", s, got)
		}
	}
}

func TestTLV(t *testing.T) {
	enc := EncodeTLV(0xBF2D, EncodeTLV(0xA0, append(EncodeTLV(0xE3, append(EncodeTLV(0x5A, []byte{0x98, 0x44}), EncodeTLV(0x9F70, []byte{1})...)), 0x00)))
	tl, err := ParseTLVs(enc)
	if err != nil || len(tl) != 1 || tl[0].Tag != 0xBF2D {
		t.Fatalf("%v %v", tl, err)
	}
	e := tl[0].Find(0xA0).Find(0xE3)
	if e == nil || e.Find(0x9F70) == nil || e.Find(0x9F70).Value[0] != 1 {
		t.Fatal("nested find failed")
	}
}
