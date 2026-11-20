package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"vowifi/esim"
)

type testAPDU func([]byte) ([]byte, error)

func (f testAPDU) Transmit(b []byte) ([]byte, error) { return f(b) }

func TestExchangeAPDU(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses [][]byte
		want      []byte
		fail      bool
	}{
		{"success", [][]byte{{1, 2, 0x90, 0}}, []byte{1, 2}, false},
		{"response chain", [][]byte{{1, 0x61, 2}, {2, 3, 0x90, 0}}, []byte{1, 2, 3}, false},
		{"correct Le", [][]byte{{0x6c, 3}, {1, 2, 3, 0x90, 0}}, []byte{1, 2, 3}, false},
		{"denied", [][]byte{{0x69, 0x82}}, nil, true},
		{"truncated", [][]byte{{0x90}}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			ch := testAPDU(func(cmd []byte) ([]byte, error) {
				if n >= len(tc.responses) {
					t.Fatal("extra APDU")
				}
				if n == 1 && tc.name == "response chain" && !bytes.Equal(cmd, []byte{0x80, 0xc0, 0, 0, 2}) {
					t.Fatalf("GET RESPONSE %x", cmd)
				}
				if n == 1 && tc.name == "correct Le" && cmd[4] != 3 {
					t.Fatal("Le not corrected")
				}
				r := tc.responses[n]
				n++
				return r, nil
			})
			got, err := exchangeAPDU(context.Background(), ch, []byte{0x80, 0xca, 0, 0, 0})
			if (err != nil) != tc.fail || !bytes.Equal(got, tc.want) {
				t.Fatalf("got %x err=%v", got, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := exchangeAPDU(ctx, testAPDU(func([]byte) ([]byte, error) { t.Fatal("sent after cancellation"); return nil, nil }), []byte{0, 0, 0, 0}); err == nil {
		t.Fatal("cancel ignored")
	}
	count := 0
	if _, err := exchangeAPDU(context.Background(), testAPDU(func([]byte) ([]byte, error) { count++; return []byte{0x61, 1}, nil }), []byte{0, 0, 0, 0}); err == nil || count != 64 {
		t.Fatal("chain limit ignored")
	}
}

func TestReadEsimAuthorisedChannel(t *testing.T) {
	wrap := func(tag uint32, body []byte) []byte { return append(esim.EncodeTLV(tag, body), 0x90, 0) }
	calls := 0
	ch := testAPDU(func(cmd []byte) ([]byte, error) {
		calls++
		if len(cmd) < 8 || cmd[1] != 0xe2 {
			t.Fatalf("unexpected command %x", cmd)
		}
		switch cmd[6] {
		case 0x3e:
			return wrap(0xbf3e, esim.EncodeTLV(0x5a, bytes.Repeat([]byte{0x11}, 16))), nil
		case 0x22:
			info := append(esim.EncodeTLV(0x82, []byte{2, 2, 2}), esim.EncodeTLV(0x83, []byte{4, 2, 0})...)
			return wrap(0xbf22, info), nil
		case 0x3c:
			return wrap(0xbf3c, nil), nil
		case 0x2d:
			p := append(esim.EncodeTLV(0x5a, bytes.Repeat([]byte{0}, 10)), esim.EncodeTLV(0x9f70, []byte{1})...)
			p = append(p, esim.EncodeTLV(0x91, []byte("Test provider"))...)
			return wrap(0xbf2d, esim.EncodeTLV(0xa0, esim.EncodeTLV(0xe3, p))), nil
		default:
			t.Fatalf("unexpected ES10 tag %02X", cmd[6])
			return nil, nil
		}
	})
	raw, err := ReadEsimChannelJSON(ch, "GSMA · OMAPI")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Info     esim.Info
		Profiles []esim.Profile
		Source   string
		ReadOnly bool
	}
	if err = json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Info.EID) != 32 || result.Info.SGP22Version != "2.2.2" || result.Info.FirmwareVersion != "4.2.0" || len(result.Profiles) != 1 || !result.Profiles[0].Enabled || result.ReadOnly || result.Source != "omapi" || calls != 4 {
		t.Fatal("metadata mapping failed")
	}
	if _, err := ReadEsimChannelJSON(testAPDU(func([]byte) ([]byte, error) { return nil, errors.New("denied") }), "test"); err == nil {
		t.Fatal("transport failure ignored")
	}
}
