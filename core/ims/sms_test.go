package ims

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestGSM7Pack(t *testing.T) {
	s, ok := gsm7Encode("hellohello")
	if !ok {
		t.Fatal("not gsm7")
	}
	if got := strings.ToUpper(hex.EncodeToString(packSeptets(s, 0))); got != "E8329BFD4697D9EC37" {
		t.Fatalf("packed %s", got)
	}
	if gsm7Decode(unpackSeptets(packSeptets(s, 0), len(s), 0)) != "hellohello" {
		t.Fatal("round trip")
	}
}

// Build SUBMITs and re-parse them as DELIVERs (same UD layout).
func TestSubmitRoundTrip(t *testing.T) {
	mr := byte(0)
	next := func() byte { mr++; return mr }
	for _, text := range []string{"hi {there} €5", strings.Repeat("长短信测试😀", 30), strings.Repeat("abc", 120)} {
		parts := buildSubmits("+447700900123", text, false, next)
		var joined string
		for _, tp := range parts {
			// Convert SUBMIT to DELIVER: drop MR and VP, add SCTS.
			first := tp[0]&0x40 | 0x00
			da := tp[2 : 4+(int(tp[2])+1)/2]
			rest := tp[4+(int(tp[2])+1)/2:]
			pid, dcs, ud := rest[0], rest[1], rest[3:]
			deliver := append([]byte{first}, da...)
			deliver = append(deliver, pid, dcs, 0x62, 0x11, 0x91, 0x21, 0x43, 0x65, 0x00)
			deliver = append(deliver, ud...)
			d, err := parseDeliver(deliver)
			if err != nil {
				t.Fatal(err)
			}
			if d.from != "+447700900123" {
				t.Fatalf("from %q", d.from)
			}
			joined += d.text
		}
		if joined != text {
			t.Fatalf("round trip mismatch (%d parts):\n%q\n%q", len(parts), joined, text)
		}
	}
}
