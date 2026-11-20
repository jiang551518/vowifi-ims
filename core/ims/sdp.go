package ims

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Codec describes the negotiated audio codec.
type Codec struct {
	Name       string // "AMR-WB" or "AMR"
	PT         int
	Rate       int
	OctetAlign bool
	ModeSet    []int // allowed FT values, empty = all
	DTMFPT     int   // 0 = none
	DTMFRate   int
}

// MaxMode returns the highest allowed frame type.
func (c Codec) MaxMode() int {
	max := 8
	if c.Name == "AMR" {
		max = 7
	}
	if len(c.ModeSet) == 0 {
		return max
	}
	m := 0
	for _, v := range c.ModeSet {
		if v > m && v <= max {
			m = v
		}
	}
	return m
}

// sdpMedia is the subset of an audio description we care about.
type sdpMedia struct {
	addr      net.IP
	port      int
	formats   []int
	rtpmap    map[int]string // pt -> "AMR-WB/16000/1"
	fmtp      map[int]string
	direction string // sendrecv, sendonly, recvonly, inactive
	precond   bool   // has a=des:qos
}

func parseSDP(body []byte) (*sdpMedia, error) {
	m := &sdpMedia{rtpmap: map[int]string{}, fmtp: map[int]string{}, direction: "sendrecv"}
	var sessionAddr net.IP
	inAudio := false
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		k, v := line[0], line[2:]
		switch k {
		case 'c':
			f := strings.Fields(v)
			if len(f) == 3 {
				ip := net.ParseIP(strings.Split(f[2], "/")[0])
				if inAudio {
					m.addr = ip
				} else {
					sessionAddr = ip
				}
			}
		case 'm':
			f := strings.Fields(v)
			inAudio = len(f) >= 3 && f[0] == "audio" && m.port == 0
			if inAudio {
				m.port, _ = strconv.Atoi(f[1])
				for _, x := range f[3:] {
					if n, err := strconv.Atoi(x); err == nil {
						m.formats = append(m.formats, n)
					}
				}
			}
		case 'a':
			if !inAudio {
				continue
			}
			name, val, _ := strings.Cut(v, ":")
			switch name {
			case "rtpmap":
				pt, rest, _ := strings.Cut(val, " ")
				if n, err := strconv.Atoi(pt); err == nil {
					m.rtpmap[n] = rest
				}
			case "fmtp":
				pt, rest, _ := strings.Cut(val, " ")
				if n, err := strconv.Atoi(pt); err == nil {
					m.fmtp[n] = rest
				}
			case "sendrecv", "sendonly", "recvonly", "inactive":
				m.direction = name
			case "des":
				if strings.HasPrefix(val, "qos") {
					m.precond = true
				}
			}
		}
	}
	if m.addr == nil {
		m.addr = sessionAddr
	}
	if m.port == 0 || m.addr == nil {
		return nil, errors.New("sdp: no audio stream")
	}
	return m, nil
}

func fmtpParam(f, name string) (string, bool) {
	for _, p := range strings.Split(f, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// selectCodec picks AMR-WB, then AMR, from a remote description.
func (m *sdpMedia) selectCodec() (Codec, error) {
	var best *Codec
	rank := map[string]int{"AMR-WB": 2, "AMR": 1}
	for _, pt := range m.formats {
		rm := m.rtpmap[pt]
		encoding := strings.Split(rm, "/")
		name := strings.ToUpper(encoding[0])
		expectedRate := "8000"
		if name == "AMR-WB" {
			expectedRate = "16000"
		}
		if rank[name] == 0 || len(encoding) < 2 || encoding[1] != expectedRate ||
			(len(encoding) > 2 && encoding[2] != "1") {
			continue
		}
		c := Codec{Name: name, PT: pt, Rate: 8000}
		if name == "AMR-WB" {
			c.Rate = 16000
		}
		f := m.fmtp[pt]
		if v, ok := fmtpParam(f, "octet-align"); ok && v == "1" {
			c.OctetAlign = true
		}
		if v, ok := fmtpParam(f, "mode-set"); ok {
			for _, s := range strings.Split(v, ",") {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
					c.ModeSet = append(c.ModeSet, n)
				}
			}
		}
		if best == nil || rank[name] > rank[best.Name] {
			cp := c
			best = &cp
		}
	}
	if best == nil {
		return Codec{}, errors.New("sdp: no AMR/AMR-WB codec offered")
	}
	for _, pt := range m.formats {
		rm := strings.ToLower(m.rtpmap[pt])
		if strings.HasPrefix(rm, "telephone-event/"+strconv.Itoa(best.Rate)) {
			best.DTMFPT, best.DTMFRate = pt, best.Rate
		}
	}
	return *best, nil
}

type sdpBuilder struct {
	sessID  int64
	version int
}

func newSDPBuilder() *sdpBuilder { return &sdpBuilder{sessID: time.Now().UnixNano() / 1e6} }

func ipFamily(ip net.IP) string {
	if ip.To4() != nil {
		return "IP4"
	}
	return "IP6"
}

// offer builds our initial offer.
func (b *sdpBuilder) offer(ip net.IP, port int) []byte {
	b.version++
	fam := ipFamily(ip)
	var s strings.Builder
	fmt.Fprintf(&s, "v=0\r\no=- %d %d IN %s %s\r\ns=-\r\nc=IN %s %s\r\nb=AS:49\r\nt=0 0\r\n", b.sessID, b.version, fam, ip, fam, ip)
	fmt.Fprintf(&s, "m=audio %d RTP/AVP 104 102 105 100\r\nb=AS:49\r\n", port)
	s.WriteString("a=rtpmap:104 AMR-WB/16000/1\r\na=fmtp:104 mode-change-capability=2;max-red=0\r\n")
	s.WriteString("a=rtpmap:102 AMR/8000/1\r\na=fmtp:102 mode-change-capability=2;max-red=0\r\n")
	s.WriteString("a=rtpmap:105 telephone-event/16000\r\na=fmtp:105 0-15\r\n")
	s.WriteString("a=rtpmap:100 telephone-event/8000\r\na=fmtp:100 0-15\r\n")
	s.WriteString("a=ptime:20\r\na=maxptime:240\r\na=sendrecv\r\n")
	return []byte(s.String())
}

// answer builds an answer to a remote offer for the selected codec.
func (b *sdpBuilder) answer(ip net.IP, port int, remote *sdpMedia, c Codec, qosMet bool) []byte {
	b.version++
	fam := ipFamily(ip)
	var s strings.Builder
	fmt.Fprintf(&s, "v=0\r\no=- %d %d IN %s %s\r\ns=-\r\nc=IN %s %s\r\nb=AS:49\r\nt=0 0\r\n", b.sessID, b.version, fam, ip, fam, ip)
	pts := strconv.Itoa(c.PT)
	if c.DTMFPT != 0 {
		pts += " " + strconv.Itoa(c.DTMFPT)
	}
	fmt.Fprintf(&s, "m=audio %d RTP/AVP %s\r\nb=AS:49\r\n", port, pts)
	fmt.Fprintf(&s, "a=rtpmap:%d %s/%d/1\r\n", c.PT, c.Name, c.Rate)
	if f := remote.fmtp[c.PT]; f != "" {
		fmt.Fprintf(&s, "a=fmtp:%d %s\r\n", c.PT, f)
	}
	if c.DTMFPT != 0 {
		fmt.Fprintf(&s, "a=rtpmap:%d telephone-event/%d\r\na=fmtp:%d 0-15\r\n", c.DTMFPT, c.DTMFRate, c.DTMFPT)
	}
	s.WriteString("a=ptime:20\r\na=maxptime:240\r\n")
	if remote.precond {
		// Our access (Wi-Fi/ePDG) has no QoS reservation: local resources are
		// available immediately.
		s.WriteString("a=curr:qos local sendrecv\r\n")
		if qosMet {
			s.WriteString("a=curr:qos remote sendrecv\r\n")
		} else {
			s.WriteString("a=curr:qos remote none\r\n")
		}
		s.WriteString("a=des:qos mandatory local sendrecv\r\na=des:qos mandatory remote sendrecv\r\n")
		if !qosMet {
			s.WriteString("a=conf:qos remote sendrecv\r\n")
		}
	}
	dir := "sendrecv"
	switch remote.direction {
	case "sendonly":
		dir = "recvonly"
	case "recvonly":
		dir = "sendonly"
	case "inactive":
		dir = "inactive"
	}
	s.WriteString("a=" + dir + "\r\n")
	return []byte(s.String())
}
