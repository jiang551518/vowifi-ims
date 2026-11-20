package ims

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"vowifi/netio"
)

func TestSDPOffer(t *testing.T) {
	for _, addr := range []string{"192.0.2.1", "2001:db8::1"} {
		b := &sdpBuilder{sessID: 123}
		offer := b.offer(net.ParseIP(addr), 40000)
		m, err := parseSDP(offer)
		if err != nil {
			t.Fatal(err)
		}
		c, err := m.selectCodec()
		if err != nil {
			t.Fatal(err)
		}
		if c.Name != "AMR-WB" || c.Rate != 16000 || c.OctetAlign || c.DTMFPT != 105 || m.port != 40000 || !m.addr.Equal(net.ParseIP(addr)) {
			t.Fatalf("offer mismatch: %+v", c)
		}
		if !strings.Contains(string(offer), "o=- 123 1 IN "+ipFamily(net.ParseIP(addr))) {
			t.Fatal("missing origin")
		}
		if !strings.Contains(string(b.offer(net.ParseIP(addr), 40000)), "o=- 123 2 IN ") {
			t.Fatal("SDP version did not increase")
		}
	}
}

func TestSDPAnswerNegotiation(t *testing.T) {
	for _, direction := range []struct{ offer, answer string }{{"sendrecv", "sendrecv"}, {"sendonly", "recvonly"}, {"recvonly", "sendonly"}, {"inactive", "inactive"}} {
		body := "v=0\r\nc=IN IP6 2001:db8::10\r\nm=audio 41000 RTP/AVP 96 97 98 99\r\na=rtpmap:96 AMR/8000/1\r\na=rtpmap:97 AMR-WB/16000/1\r\na=fmtp:97 octet-align=1; mode-set=0,2,4\r\na=rtpmap:98 telephone-event/8000\r\na=rtpmap:99 telephone-event/16000\r\na=des:qos mandatory local sendrecv\r\na=" + direction.offer + "\r\n"
		m, err := parseSDP([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		c, err := m.selectCodec()
		if err != nil {
			t.Fatal(err)
		}
		if c.Name != "AMR-WB" || c.PT != 97 || !c.OctetAlign || c.MaxMode() != 4 || !reflect.DeepEqual(c.ModeSet, []int{0, 2, 4}) || c.DTMFPT != 99 {
			t.Fatalf("bad negotiation: %+v", c)
		}
		for _, qos := range []bool{false, true} {
			answer := newSDPBuilder().answer(net.ParseIP("2001:db8::20"), 42000, m, c, qos)
			a, err := parseSDP(answer)
			if err != nil {
				t.Fatal(err)
			}
			if a.direction != direction.answer || !reflect.DeepEqual(a.formats, []int{97, 99}) || a.fmtp[97] != m.fmtp[97] {
				t.Fatalf("answer changed negotiation: %s", answer)
			}
			want := "a=curr:qos remote none"
			if qos {
				want = "a=curr:qos remote sendrecv"
			}
			if !bytes.Contains(answer, []byte(want)) || bytes.Contains(answer, []byte("a=conf:qos remote sendrecv")) == qos {
				t.Fatalf("bad preconditions: %s", answer)
			}
		}
	}
}

func TestSDPFallbackAndRejections(t *testing.T) {
	for _, tc := range []struct {
		mapValue, fmtp string
		valid          bool
	}{{"AMR/8000/1", "mode-set=0,7", true}, {"AMR/16000/1", "", false}, {"AMR-WB/8000/1", "", false}, {"PCMU/8000", "", false}} {
		m, err := parseSDP([]byte("c=IN IP4 192.0.2.1\r\nm=audio 42000 RTP/AVP 96\r\na=rtpmap:96 " + tc.mapValue + "\r\na=fmtp:96 " + tc.fmtp + "\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		c, err := m.selectCodec()
		if (err == nil) != tc.valid {
			t.Fatalf("codec %s result: %+v %v", tc.mapValue, c, err)
		}
		if tc.valid && (c.Name != "AMR" || c.MaxMode() != 7) {
			t.Fatal(c)
		}
	}
	for _, body := range []string{"", "c=IN IP4 192.0.2.1\r\nm=audio 0 RTP/AVP 96\r\n", "m=audio 41000 RTP/AVP 96\r\n"} {
		if _, err := parseSDP([]byte(body)); err == nil {
			t.Fatalf("accepted invalid SDP %q", body)
		}
	}
}

func TestAMRKnownPayloads(t *testing.T) {
	for _, align := range []bool{false, true} {
		want := []byte{0xf7, 0xc0}
		if align {
			want = []byte{0xf0, 0x7c}
		}
		got := framesToPayload([][]byte{{0x7c}}, false, align, 15)
		if !bytes.Equal(got, want) {
			t.Fatalf("NO_DATA wire format: %x != %x", got, want)
		}
		frames, err := payloadToFrames(want, false, align)
		if err != nil || !reflect.DeepEqual(frames, [][]byte{{0x7c}}) {
			t.Fatalf("NO_DATA decode: %x %v", frames, err)
		}
		want = make([]byte, 14)
		want[0] = 0xf0
		want[1] = 0x40
		if align {
			want[1] = 0x04
		}
		frame := make([]byte, 13)
		frame[0] = 4
		if got := framesToPayload([][]byte{frame}, false, align, 15); !bytes.Equal(got, want) {
			t.Fatalf("AMR FT0 wire format %x != %x", got, want)
		}
	}
}

func TestAMRFramesRoundTrip(t *testing.T) {
	for _, wb := range []bool{false, true} {
		for _, align := range []bool{false, true} {
			modes := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 15}
			if wb {
				modes = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 14, 15}
			}
			var frames [][]byte
			for _, ft := range modes {
				bits := frameBits(wb, ft)
				f := make([]byte, 1+(bits+7)/8)
				f[0] = byte(ft<<3 | 4)
				for i := 1; i < len(f); i++ {
					f[i] = byte(i*37 + ft)
				}
				if bits%8 != 0 {
					f[len(f)-1] &= byte(0xff << uint(8-bits%8))
				}
				frames = append(frames, f)
			}
			p := framesToPayload(frames, wb, align, 15)
			got, err := payloadToFrames(p, wb, align)
			if err != nil || !reflect.DeepEqual(got, frames) {
				t.Fatalf("wb=%v align=%v round trip: %v", wb, align, err)
			}
			// Every nonempty speech frame must reject truncation.
			for _, f := range frames {
				if len(f) > 1 {
					p = framesToPayload([][]byte{f}, wb, align, 15)
					if _, err := payloadToFrames(p[:len(p)-1], wb, align); err == nil {
						t.Fatal("truncated speech accepted")
					}
				}
			}
		}
	}
	for _, p := range [][]byte{nil, {0xf0}, {0xf0, 0x80}} {
		if _, err := payloadToFrames(p, false, true); err == nil {
			t.Fatalf("truncated TOC accepted: %x", p)
		}
	}
}

func mediaTestPair(t *testing.T) (*netio.Stack, *netio.Stack) {
	t.Helper()
	a, b := &memoryLink{}, &memoryLink{}
	a.peer = b
	b.peer = a
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	local, err := netio.New(a, net.ParseIP("10.0.0.1"), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(local.Close)
	remote, err := netio.New(b, net.ParseIP("10.0.0.2"), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	return local, remote
}

func TestRFC4733WirePackets(t *testing.T) {
	local, remote := mediaTestPair(t)
	sender, err := local.ListenUDP(net.ParseIP("10.0.0.1"), 40000)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := remote.ListenUDP(net.ParseIP("10.0.0.2"), 41000)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	s := &rtpSession{conn: sender, remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.2"), Port: 41000}, codec: Codec{DTMFPT: 105, DTMFRate: 16000}, ssrc: 0x12345678, seq: 100, ts: 9600}
	packets := make(chan []byte, 8)
	go func() {
		b := make([]byte, 1500)
		for i := 0; i < 8; i++ {
			n, _, err := receiver.ReadFrom(b)
			if err != nil {
				return
			}
			packets <- append([]byte(nil), b[:n]...)
		}
	}()
	if err := s.sendDTMF('#'); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 8; i++ {
		select {
		case p := <-packets:
			if len(p) != 16 || p[0] != 0x80 || int(p[1]&0x7f) != 105 || (p[1]&0x80 != 0) != (i == 1) || binary.BigEndian.Uint16(p[2:]) != uint16(100+i) || binary.BigEndian.Uint32(p[4:]) != 9600 || binary.BigEndian.Uint32(p[8:]) != 0x12345678 || p[12] != 11 || p[13]&0x7f != 10 {
				t.Fatalf("bad DTMF packet %d: %x", i, p)
			}
			if (p[13]&0x80 != 0) != (i >= 6) {
				t.Fatalf("end flag %d", i)
			}
			duration := i * 320
			if i >= 6 {
				duration = 1600
			}
			if int(binary.BigEndian.Uint16(p[14:])) != duration {
				t.Fatalf("duration %d", i)
			}
		case <-time.After(time.Second):
			t.Fatal("missing DTMF packet")
		}
	}
	if s.stats().TxPackets != 8 {
		t.Fatal("DTMF packets missing from stats")
	}
	if err := s.sendDTMF('x'); err == nil {
		t.Fatal("invalid DTMF accepted")
	}
	s.codec.DTMFPT = 0
	if err := s.sendDTMF('1'); err == nil {
		t.Fatal("unnegotiated DTMF accepted")
	}
}

func TestRTPAudioAndStats(t *testing.T) {
	local, remote := mediaTestPair(t)
	sender, err := local.ListenUDP(net.ParseIP("10.0.0.1"), 40000)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := remote.ListenUDP(net.ParseIP("10.0.0.2"), 41000)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	codec := Codec{Name: "AMR-WB", PT: 104, Rate: 16000, OctetAlign: true}
	frames := make(chan AudioFrame, 2)
	rx := &rtpSession{conn: receiver, codec: codec, onRecv: func(f AudioFrame) { frames <- f }}
	go rx.readLoop()
	tx := &rtpSession{conn: sender, codec: codec, remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.2"), Port: 41000}, marker: true, seq: 10, ts: 640}
	for i := 0; i < 2; i++ {
		if err := tx.sendFrame([]byte{0x7c}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case f := <-frames:
			if f.Seq != uint16(11+i) || f.TS != uint32(640+320*i) || !bytes.Equal(f.Frame, []byte{0x7c}) {
				t.Fatal(f)
			}
		case <-time.After(time.Second):
			t.Fatal("audio missing")
		}
	}
	ua := &UA{calls: &callState{calls: map[string]*dialog{"test": {info: CallInfo{ID: "test"}, rtp: rx}}}}
	stats, err := ua.CallMediaStats("test")
	if err != nil || stats.RxPackets != 2 || tx.stats().TxPackets != 2 {
		t.Fatalf("bad stats: %+v %v", stats, err)
	}
	if _, err := ua.CallMediaStats("missing"); err == nil {
		t.Fatal("missing call accepted")
	}
	sender.Close()
	if err := tx.sendFrame([]byte{0x7c}); err == nil {
		t.Fatal("closed sender accepted")
	}
	if tx.stats().TxPackets != 2 {
		t.Fatal("failed packet counted as sent")
	}
}

func TestRFC4733ReceiveDedup(t *testing.T) {
	local, remote := mediaTestPair(t)
	sender, err := local.ListenUDP(net.ParseIP("10.0.0.1"), 40000)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := remote.ListenUDP(net.ParseIP("10.0.0.2"), 41000)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	digits := make(chan rune, 8)
	rx := &rtpSession{conn: receiver, codec: Codec{PT: 104, DTMFPT: 105}, onDTMF: func(d rune) { digits <- d }}
	go rx.readLoop()
	// Zero is a valid initial RTP timestamp. Repeated end packets must not
	// emit duplicate keypad events, and a later event must still be delivered.
	for i := 0; i < 4; i++ {
		p := make([]byte, 16)
		p[0] = 0x80
		p[1] = 105
		p[12] = 1
		p[13] = 0x8a
		binary.BigEndian.PutUint16(p[2:], uint16(i+1))
		binary.BigEndian.PutUint16(p[14:], 800)
		if i == 3 {
			binary.BigEndian.PutUint32(p[4:], 1600)
			p[12] = 11
		}
		if _, err := sender.WriteTo(p, &net.UDPAddr{IP: net.ParseIP("10.0.0.2"), Port: 41000}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []rune{'1', '#'} {
		select {
		case got := <-digits:
			if got != want {
				t.Fatalf("got %c want %c", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("event missing")
		}
	}
	select {
	case got := <-digits:
		t.Fatalf("duplicate DTMF event %c", got)
	case <-time.After(50 * time.Millisecond):
	}
}
