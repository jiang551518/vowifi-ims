package ims

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

// Speech bits per frame type (RFC 4867 §3.6 / TS 26.101, 26.201).
var (
	amrNBBits = []int{95, 103, 118, 134, 148, 159, 204, 244, 39}
	amrWBBits = []int{132, 177, 253, 285, 317, 365, 397, 461, 477, 40}
)

func frameBits(wb bool, ft int) int {
	tbl := amrNBBits
	if wb {
		tbl = amrWBBits
	}
	if ft >= 0 && ft < len(tbl) {
		return tbl[ft]
	}
	return 0 // NO_DATA (15) and reserved types
}

type bitReader struct {
	b   []byte
	pos int
}

func (r *bitReader) read(n int) (uint32, bool) {
	var v uint32
	for i := 0; i < n; i++ {
		if r.pos/8 >= len(r.b) {
			return 0, false
		}
		v = v<<1 | uint32(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v, true
}

type bitWriter struct {
	b   []byte
	pos int
}

func (w *bitWriter) write(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.pos/8 >= len(w.b) {
			w.b = append(w.b, 0)
		}
		if v>>i&1 != 0 {
			w.b[w.pos/8] |= 1 << (7 - w.pos%8)
		}
		w.pos++
	}
}

// payloadToFrames splits an RTP AMR payload into storage-format frames
// (one header byte "0 FT Q 00" followed by speech bits, byte padded).
func payloadToFrames(p []byte, wb, octetAlign bool) ([][]byte, error) {
	r := &bitReader{b: p}
	if _, ok := r.read(4); !ok { // CMR
		return nil, errors.New("amr: empty payload")
	}
	if octetAlign {
		r.read(4)
	}
	type toc struct{ ft, q int }
	var tocs []toc
	for {
		f, ok1 := r.read(1)
		ft, ok2 := r.read(4)
		q, ok3 := r.read(1)
		if !ok1 || !ok2 || !ok3 {
			return nil, errors.New("amr: truncated TOC")
		}
		if octetAlign {
			r.read(2)
		}
		tocs = append(tocs, toc{int(ft), int(q)})
		if f == 0 {
			break
		}
		if len(tocs) > 12 {
			return nil, errors.New("amr: too many frames")
		}
	}
	var out [][]byte
	for _, t := range tocs {
		bits := frameBits(wb, t.ft)
		w := &bitWriter{b: []byte{byte(t.ft<<3 | t.q<<2)}, pos: 8}
		for i := 0; i < bits; i++ {
			v, ok := r.read(1)
			if !ok {
				return nil, errors.New("amr: truncated speech data")
			}
			w.write(v, 1)
		}
		if octetAlign && r.pos%8 != 0 {
			r.pos += 8 - r.pos%8
		}
		out = append(out, w.b[:1+(bits+7)/8])
	}
	return out, nil
}

// framesToPayload packs storage-format frames into one RTP payload.
func framesToPayload(frames [][]byte, wb, octetAlign bool, cmr int) []byte {
	w := &bitWriter{}
	w.write(uint32(cmr), 4)
	if octetAlign {
		w.write(0, 4)
	}
	for i, f := range frames {
		ft, q := int(f[0]>>3&0x0f), int(f[0]>>2&1)
		more := uint32(0)
		if i < len(frames)-1 {
			more = 1
		}
		w.write(more, 1)
		w.write(uint32(ft), 4)
		w.write(uint32(q), 1)
		if octetAlign {
			w.write(0, 2)
		}
	}
	for _, f := range frames {
		ft := int(f[0] >> 3 & 0x0f)
		bits := frameBits(wb, ft)
		r := &bitReader{b: f[1:]}
		for i := 0; i < bits; i++ {
			v, _ := r.read(1)
			w.write(v, 1)
		}
		if octetAlign && w.pos%8 != 0 {
			w.write(0, 8-w.pos%8)
		}
	}
	return w.b
}

// AudioFrame is one received AMR frame in storage format.
type AudioFrame struct {
	Seq   uint16
	TS    uint32
	Frame []byte
}

// rtpSession sends and receives one audio stream over the inner stack.
type rtpSession struct {
	conn     *gonet.UDPConn
	port     int
	mu       sync.Mutex
	remote   *net.UDPAddr
	codec    Codec
	ssrc     uint32
	seq      uint16
	ts       uint32
	marker   bool
	onRecv   func(AudioFrame)
	onDTMF   func(rune)
	closed   atomic.Bool
	lastRx   atomic.Int64
	rxPkts   atomic.Uint64
	txPkts   atomic.Uint64
	dtmfLast uint32
	dtmfSeen bool
}

func newRTPSession(ua *UA) (*rtpSession, error) {
	var lastErr error
	for i := 0; i < 10; i++ {
		port := randomPort()
		c, err := ua.cfg.Stack.ListenUDP(ua.tr.Local, port)
		if err != nil {
			lastErr = err
			continue
		}
		var b [6]byte
		_, _ = rand.Read(b[:])
		s := &rtpSession{conn: c, port: port, ssrc: binary.BigEndian.Uint32(b[:]),
			seq: binary.BigEndian.Uint16(b[4:]), marker: true}
		go s.readLoop()
		return s, nil
	}
	return nil, lastErr
}

func (s *rtpSession) configure(remote *sdpMedia, c Codec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remote = &net.UDPAddr{IP: remote.addr, Port: remote.port}
	s.codec = c
}

func (s *rtpSession) close() {
	if s.closed.CompareAndSwap(false, true) {
		s.conn.Close()
	}
}

func (s *rtpSession) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, _, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		p := buf[:n]
		if n < 12 || p[0]>>6 != 2 {
			continue
		}
		cc := int(p[0] & 0x0f)
		off := 12 + 4*cc
		if p[0]&0x10 != 0 && len(p) >= off+4 { // header extension
			off += 4 + 4*int(binary.BigEndian.Uint16(p[off+2:]))
		}
		if off > n {
			continue
		}
		end := n
		if p[0]&0x20 != 0 { // padding
			end -= int(p[n-1])
		}
		if end < off {
			continue
		}
		pt := int(p[1] & 0x7f)
		seq := binary.BigEndian.Uint16(p[2:])
		ts := binary.BigEndian.Uint32(p[4:])
		s.lastRx.Store(time.Now().UnixNano())
		s.rxPkts.Add(1)
		s.mu.Lock()
		c := s.codec
		onRecv, onDTMF := s.onRecv, s.onDTMF
		s.mu.Unlock()
		payload := p[off:end]
		switch {
		case pt == c.PT:
			frames, err := payloadToFrames(payload, c.Name == "AMR-WB", c.OctetAlign)
			if err != nil || onRecv == nil {
				continue
			}
			step := uint32(c.Rate / 50)
			for i, f := range frames {
				onRecv(AudioFrame{Seq: seq, TS: ts + uint32(i)*step, Frame: append([]byte(nil), f...)})
			}
		case pt == c.DTMFPT && len(payload) >= 4:
			// RFC 4733: report each event once, on the first packet seen.
			if (!s.dtmfSeen || ts != s.dtmfLast) && onDTMF != nil {
				s.dtmfSeen = true
				s.dtmfLast = ts
				ev := payload[0]
				if ev < 16 {
					onDTMF(rune("0123456789*#ABCD"[ev]))
				}
			}
		}
	}
}

// sendFrame packetises one 20 ms storage-format frame.
func (s *rtpSession) sendFrame(frame []byte) error {
	s.mu.Lock()
	remote, c := s.remote, s.codec
	if remote == nil || c.PT == 0 && c.Name == "" {
		s.mu.Unlock()
		return nil
	}
	s.seq++
	hdr := make([]byte, 12)
	hdr[0] = 0x80
	hdr[1] = byte(c.PT)
	if s.marker {
		hdr[1] |= 0x80
		s.marker = false
	}
	binary.BigEndian.PutUint16(hdr[2:], s.seq)
	binary.BigEndian.PutUint32(hdr[4:], s.ts)
	binary.BigEndian.PutUint32(hdr[8:], s.ssrc)
	s.ts += uint32(c.Rate / 50)
	s.mu.Unlock()
	pkt := append(hdr, framesToPayload([][]byte{frame}, c.Name == "AMR-WB", c.OctetAlign, 15)...)
	_, err := s.conn.WriteTo(pkt, remote)
	if err == nil {
		s.txPkts.Add(1)
	}
	return err
}

// sendDTMF sends an RFC 4733 event of 100 ms.
func (s *rtpSession) sendDTMF(digit rune) error {
	idx := -1
	for i, r := range "0123456789*#ABCD" {
		if r == digit {
			idx = i
		}
	}
	s.mu.Lock()
	remote, c := s.remote, s.codec
	s.mu.Unlock()
	if idx < 0 || remote == nil || c.DTMFPT == 0 {
		return errors.New("ims: DTMF not negotiated")
	}
	s.mu.Lock()
	start := s.ts
	s.mu.Unlock()
	step := c.DTMFRate / 50
	for i := 1; i <= 8; i++ {
		end := i >= 6
		dur := uint16(i * step)
		if end {
			dur = uint16(5 * step)
		}
		s.mu.Lock()
		s.seq++
		seq := s.seq
		s.mu.Unlock()
		pkt := make([]byte, 16)
		pkt[0] = 0x80
		pkt[1] = byte(c.DTMFPT)
		if i == 1 {
			pkt[1] |= 0x80
		}
		binary.BigEndian.PutUint16(pkt[2:], seq)
		binary.BigEndian.PutUint32(pkt[4:], start)
		binary.BigEndian.PutUint32(pkt[8:], s.ssrc)
		pkt[12] = byte(idx)
		pkt[13] = 10 // volume
		if end {
			pkt[13] |= 0x80
		}
		binary.BigEndian.PutUint16(pkt[14:], dur)
		if _, err := s.conn.WriteTo(pkt, remote); err != nil {
			return err
		}
		s.txPkts.Add(1)
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func (s *rtpSession) stats() MediaStats {
	return MediaStats{RxPackets: s.rxPkts.Load(), TxPackets: s.txPkts.Load()}
}
