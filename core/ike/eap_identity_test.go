package ike

import (
	"context"
	"encoding/binary"
	"testing"
)

// An AKA'-Identity request must be answered with the "6"-prefixed
// permanent identity (TS 23.003 §19.3.2); EAP-AKA keeps "0".
func TestAKAIdentityPrefix(t *testing.T) {
	for _, tc := range []struct {
		typ  byte
		want byte
	}{{eapTypeAKA, '0'}, {eapTypeAKAPrime, '6'}} {
		e := newEAPAKA("0454071234567890@nai.epc.mnc007.mcc454.3gppnetwork.org", nil)
		req := []byte{eapRequest, 7, 0, 0, tc.typ, akaIdentity, 0, 0}
		req = append(req, encodeEAPAttr(atPermanentIDReq, []byte{0, 0})...)
		binary.BigEndian.PutUint16(req[2:], uint16(len(req)))
		resp, _, err := e.handle(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		attrs, err := parseEAPAttrs(resp[8:])
		if err != nil {
			t.Fatal(err)
		}
		id := findAttr(attrs, atIdentity)
		if id == nil || id.value[2] != tc.want || e.identity[0] != tc.want {
			t.Fatalf("type %d: identity prefix %q, want %q", tc.typ, id.value[2], tc.want)
		}
	}
}
