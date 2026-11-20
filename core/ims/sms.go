package ims

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// SMS is a received short message (concatenated parts already joined).
type SMS struct {
	From  string    `json:"from"`
	Text  string    `json:"text"`
	Time  time.Time `json:"time"`
	SMSC  string    `json:"smsc,omitempty"`
	Parts int       `json:"parts"`
	Class int       `json:"class"` // -1 none, 0 = flash
}

// SMSStatus reports the outcome of a submitted message.
type SMSStatus struct {
	ID     string `json:"id"`
	To     string `json:"to"`
	State  string `json:"state"` // sent | failed | delivered | undelivered
	Parts  int    `json:"parts"`
	Detail string `json:"detail,omitempty"`
}

type smsState struct {
	ua   *UA
	mu   sync.Mutex
	rpMR byte
	tpMR byte
	acks map[byte]chan rpResult
	// concatenation buffers keyed by sender/ref/total
	parts map[string]*concatBuf
	// TP-MR -> submission id, for status reports
	reports map[byte]string
	seenMT  map[string]time.Time
	// raw: hand MT TPDUs to the caller (Android's SMS stack via the system
	// IMS service) and acknowledge only when it says so (AckSMS).
	raw     atomic.Bool
	pending map[string]func(ok bool, cause byte)
}

// SMSRaw is an incoming SMS for the platform SMS stack: pdu is the 3GPP
// format Android expects (SMSC address as in TS 23.040 + TPDU), hex.
type SMSRaw struct {
	ID     string `json:"id"`
	PDU    string `json:"pdu"`
	Report bool   `json:"report"`
}

// SetRawSMS switches incoming SMS to raw delivery (see SMSRaw / AckSMS).
func (ua *UA) SetRawSMS(on bool) { ua.sms.raw.Store(on) }

// AckSMS answers a raw incoming SMS: RP-ACK when ok, else RP-ERROR with the
// given RP cause. Returns false if the id is unknown (already answered).
func (ua *UA) AckSMS(id string, ok bool, cause int) bool {
	s := ua.sms
	s.mu.Lock()
	f := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if f == nil {
		return false
	}
	go f(ok, byte(cause))
	return true
}

// holdAck parks the RP acknowledgement of a raw SMS until AckSMS, answering
// with a temporary failure (SMSC retries later) if nobody does in time.
func (s *smsState) holdAck(id string, ack func(ok bool, cause byte)) {
	s.mu.Lock()
	s.pending[id] = ack
	s.mu.Unlock()
	time.AfterFunc(30*time.Second, func() {
		s.mu.Lock()
		f := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if f != nil {
			s.ua.log.Warn("sms: platform did not acknowledge, answering temporary failure", "id", id)
			f(false, 41)
		}
	})
}

// SendTPDU submits one SMS-SUBMIT TPDU built by the caller (Android's SMS
// stack) and waits for the SMSC. It returns 0 on RP-ACK, the RP cause on
// RP-ERROR, or an error when the request could not be delivered.
func (ua *UA) SendTPDU(ctx context.Context, smscAddr string, tpdu []byte) (int, error) {
	s := ua.sms
	smsc := normalizeNumber(smscAddr, ua.cfg.CountryCode)
	if smsc == "" {
		smsc = normalizeNumber(ua.cfg.SMSCAddress, ua.cfg.CountryCode)
	}
	if smsc == "" {
		return 0, errors.New("ims: no SMSC address")
	}
	if len(tpdu) == 0 || len(tpdu) > 255 {
		return 0, errors.New("ims: bad TPDU")
	}
	s.mu.Lock()
	s.rpMR++
	mr := s.rpMR
	ch := make(chan rpResult, 1)
	s.acks[mr] = ch
	s.mu.Unlock()
	rp := []byte{0x00, mr, 0x00}
	rp = append(rp, encodeRPAddress(smsc)...)
	rp = append(rp, byte(len(tpdu)))
	rp = append(rp, tpdu...)
	resp, err := ua.sendMessage(ctx, ua.smscURI(smsc), rp, "")
	if err != nil {
		s.dropAck(mr)
		return 0, err
	}
	if resp.StatusCode >= 300 {
		s.dropAck(mr)
		return 0, fmt.Errorf("ims: MESSAGE rejected %d %s", resp.StatusCode, resp.Reason)
	}
	select {
	case r := <-ch:
		if !r.ok {
			return r.cause, nil
		}
		return 0, nil
	case <-time.After(45 * time.Second):
		s.dropAck(mr)
		return 0, errors.New("ims: no RP-ACK from SMSC")
	case <-ctx.Done():
		s.dropAck(mr)
		return 0, ctx.Err()
	}
}

type rpResult struct {
	ok    bool
	cause int
}

type concatBuf struct {
	total int
	parts map[int]string
	first time.Time
	scts  time.Time
	smsc  string
}

func newSMSState(ua *UA) *smsState {
	return &smsState{ua: ua, acks: map[byte]chan rpResult{}, parts: map[string]*concatBuf{},
		reports: map[byte]string{}, seenMT: map[string]time.Time{}, pending: map[string]func(bool, byte){}}
}

// ---------------------------------------------------------------- MO

// SendSMS submits text to a number. It returns once every part has been
// accepted (RP-ACK) by the SMSC.
func (ua *UA) SendSMS(ctx context.Context, to, text string, statusReport bool) (SMSStatus, error) {
	s := ua.sms
	smsc := normalizeNumber(ua.cfg.SMSCAddress, ua.cfg.CountryCode)
	if smsc == "" {
		return SMSStatus{}, errors.New("ims: no SMSC address (read from SIM or set it in settings)")
	}
	dest := normalizeNumber(to, ua.cfg.CountryCode)
	if dest == "" {
		return SMSStatus{}, errors.New("ims: invalid recipient")
	}
	tpdus := buildSubmits(dest, text, statusReport, s.nextTPMR)
	id := randHex(6)
	st := SMSStatus{ID: id, To: dest, Parts: len(tpdus), State: "sent"}
	for i, tp := range tpdus {
		s.mu.Lock()
		s.rpMR++
		mr := s.rpMR
		ch := make(chan rpResult, 1)
		s.acks[mr] = ch
		if statusReport {
			s.reports[tp[1]] = id
		}
		s.mu.Unlock()
		rp := []byte{0x00, mr, 0x00}
		rp = append(rp, encodeRPAddress(smsc)...)
		rp = append(rp, byte(len(tp)))
		rp = append(rp, tp...)
		resp, err := ua.sendMessage(ctx, ua.smscURI(smsc), rp, "")
		if err != nil {
			s.dropAck(mr)
			return failed(st, err.Error()), err
		}
		if resp.StatusCode >= 300 {
			s.dropAck(mr)
			err := fmt.Errorf("ims: MESSAGE rejected %d %s", resp.StatusCode, resp.Reason)
			return failed(st, err.Error()), err
		}
		select {
		case r := <-ch:
			if !r.ok {
				err := fmt.Errorf("ims: SMSC rejected part %d/%d (RP cause %d)", i+1, len(tpdus), r.cause)
				return failed(st, err.Error()), err
			}
		case <-time.After(45 * time.Second):
			s.dropAck(mr)
			err := errors.New("ims: no RP-ACK from SMSC")
			return failed(st, err.Error()), err
		case <-ctx.Done():
			s.dropAck(mr)
			return failed(st, ctx.Err().Error()), ctx.Err()
		}
	}
	ua.emit("sms_status", st)
	return st, nil
}

func failed(st SMSStatus, detail string) SMSStatus {
	st.State, st.Detail = "failed", detail
	return st
}

func (s *smsState) dropAck(mr byte) {
	s.mu.Lock()
	delete(s.acks, mr)
	s.mu.Unlock()
}

func (s *smsState) nextTPMR() byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tpMR++
	return s.tpMR
}

func (ua *UA) smscURI(num string) string {
	if ua.cfg.DialURIScheme == "sip" {
		return "sip:" + num + "@" + ua.cfg.Domain + ";user=phone"
	}
	return "tel:" + num
}

// sendMessage sends a SIP MESSAGE carrying an RP message.
func (ua *UA) sendMessage(ctx context.Context, uri string, rp []byte, inReplyTo string) (*Message, error) {
	pid := ua.preferredIdentity()
	m := &Message{Method: "MESSAGE", RequestURI: uri}
	m.Add("Max-Forwards", "70")
	ua.addRoute(m, true)
	m.Add("From", "<"+pid+">;tag="+newTag())
	m.Add("To", "<"+uri+">")
	m.Add("Call-ID", randHex(12)+"@"+ua.tr.Local.String())
	m.Add("CSeq", "1 MESSAGE")
	m.Add("P-Preferred-Identity", "<"+pid+">")
	m.Add("P-Access-Network-Info", ua.cfg.PANI)
	m.Add("Request-Disposition", "no-fork")
	m.Add("Accept-Contact", "*;+g.3gpp.smsip")
	if inReplyTo != "" {
		m.Add("In-Reply-To", inReplyTo)
	}
	m.Add("Allow", "MESSAGE")
	m.Add("User-Agent", ua.cfg.UserAgent)
	ua.addSecurity(m)
	m.Add("Content-Type", "application/vnd.3gpp.sms")
	m.Body = rp
	return ua.tr.Request(ctx, m, nil)
}

// ---------------------------------------------------------------- MT

func (s *smsState) handleMessage(m *Message) {
	ua := s.ua
	ct := strings.ToLower(m.Get("Content-Type"))
	if !strings.Contains(ct, "vnd.3gpp.sms") {
		_ = ua.tr.Reply(m, NewResponse(m, 415, "Unsupported Media Type"))
		return
	}
	_ = ua.tr.Reply(m, NewResponse(m, 200, "OK"))
	rp := m.Body
	if len(rp) < 2 {
		return
	}
	mti, mr := rp[0]&0x07, rp[1]
	switch mti {
	case 0x03: // RP-ACK network -> MS
		s.resolveAck(mr, rpResult{ok: true})
	case 0x05: // RP-ERROR network -> MS
		cause := 0
		if len(rp) >= 4 {
			cause = int(rp[3] & 0x7f)
		}
		s.resolveAck(mr, rpResult{ok: false, cause: cause})
	case 0x01: // RP-DATA network -> MS
		s.handleRPData(m, rp)
	}
}

func (s *smsState) resolveAck(mr byte, r rpResult) {
	s.mu.Lock()
	ch := s.acks[mr]
	delete(s.acks, mr)
	s.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

func (s *smsState) handleRPData(m *Message, rp []byte) {
	ua := s.ua
	mr := rp[1]
	b := rp[2:]
	// RP-OA (SMSC address)
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return
	}
	oa := append([]byte(nil), b[:1+int(b[0])]...) // same layout as a TS 23.040 SC address
	smsc := decodeRPAddress(b[1 : 1+int(b[0])])
	b = b[1+int(b[0]):]
	// RP-DA (empty)
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return
	}
	b = b[1+int(b[0]):]
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return
	}
	tpdu := b[1 : 1+int(b[0])]
	ack := func(ok bool, cause byte) {
		var body []byte
		if ok {
			body = []byte{0x02, mr, 0x41, 0x02, 0x00, 0x00} // RP-ACK with empty SMS-DELIVER-REPORT
		} else {
			body = []byte{0x04, mr, 0x01, cause}
		}
		to := URIOf(m.Get("From"))
		if pai := m.Get("P-Asserted-Identity"); pai != "" {
			to = URIOf(pai)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := ua.sendMessage(ctx, to, body, m.Get("Call-ID")); err != nil {
			ua.log.Warn("sms: sending RP-ACK failed", "err", err)
		}
	}
	if len(tpdu) < 1 {
		ack(false, 111)
		return
	}
	if s.raw.Load() {
		kind := tpdu[0] & 0x03
		if kind == 0x00 || kind == 0x02 {
			if kind == 0x00 {
				if d, err := parseDeliver(tpdu); err == nil && d.simData {
					ua.log.Info("sms: declined SIM data download (OTA) message", "pid", fmt.Sprintf("%02X", d.pid))
					ack(false, 41)
					return
				}
			}
			// Network retransmission of a message already handed over.
			callID := m.Get("Call-ID")
			s.mu.Lock()
			if t, seen := s.seenMT[callID]; seen && time.Since(t) < 10*time.Minute {
				s.mu.Unlock()
				ack(true, 0)
				return
			}
			s.seenMT[callID] = time.Now()
			s.mu.Unlock()
			id := randHex(6)
			s.holdAck(id, ack)
			ua.emit("sms_raw", SMSRaw{ID: id, PDU: hex.EncodeToString(append(oa, tpdu...)), Report: kind == 0x02})
			return
		}
	}
	switch tpdu[0] & 0x03 {
	case 0x00: // SMS-DELIVER
		d, err := parseDeliver(tpdu)
		if err != nil {
			ua.log.Warn("sms: bad SMS-DELIVER", "err", err)
			ack(false, 111)
			return
		}
		if d.simData {
			// Operator OTA for the SIM card. The app cannot hand it to the
			// UICC (ENVELOPE SMS-PP DOWNLOAD), so it must neither show it as
			// a text nor acknowledge it as delivered: a temporary failure lets
			// the SMSC retry later, e.g. through the phone's own modem.
			ua.log.Info("sms: declined SIM data download (OTA) message", "pid", fmt.Sprintf("%02X", d.pid), "class", d.class)
			ack(false, 41)
			return
		}
		ack(true, 0)
		s.deliver(d, smsc, m.Get("Call-ID"))
	case 0x02: // SMS-STATUS-REPORT
		ack(true, 0)
		if len(tpdu) >= 2 {
			s.mu.Lock()
			id := s.reports[tpdu[1]]
			s.mu.Unlock()
			if id != "" {
				state := "delivered"
				if st := statusOf(tpdu); st >= 0x20 {
					state = "undelivered"
				}
				ua.emit("sms_status", SMSStatus{ID: id, State: state})
			}
		}
	default:
		ack(true, 0)
	}
}

func statusOf(tpdu []byte) int {
	// MTI | MR | RA(len digits, toa, bcd) | SCTS(7) | DT(7) | ST
	if len(tpdu) < 4 {
		return 0
	}
	n := int(tpdu[2])
	off := 4 + (n+1)/2 + 14
	if off < len(tpdu) {
		return int(tpdu[off])
	}
	return 0
}

func (s *smsState) deliver(d deliverPDU, smsc, callID string) {
	ua := s.ua
	s.mu.Lock()
	// Duplicate suppression (network retransmissions).
	key := callID
	if t, ok := s.seenMT[key]; ok && time.Since(t) < 10*time.Minute {
		s.mu.Unlock()
		return
	}
	s.seenMT[key] = time.Now()
	for k, t := range s.seenMT {
		if time.Since(t) > time.Hour {
			delete(s.seenMT, k)
		}
	}
	if d.concatTotal <= 1 {
		s.mu.Unlock()
		ua.emit("sms", SMS{From: d.from, Text: d.text, Time: d.scts, SMSC: smsc, Parts: 1, Class: d.class})
		return
	}
	ck := fmt.Sprintf("%s/%d/%d", d.from, d.concatRef, d.concatTotal)
	buf := s.parts[ck]
	if buf == nil {
		buf = &concatBuf{total: d.concatTotal, parts: map[int]string{}, first: time.Now(), scts: d.scts, smsc: smsc}
		s.parts[ck] = buf
	}
	buf.parts[d.concatSeq] = d.text
	if len(buf.parts) < buf.total {
		s.mu.Unlock()
		return
	}
	delete(s.parts, ck)
	var sb strings.Builder
	for i := 1; i <= buf.total; i++ {
		sb.WriteString(buf.parts[i])
	}
	s.mu.Unlock()
	ua.emit("sms", SMS{From: d.from, Text: sb.String(), Time: buf.scts, SMSC: smsc, Parts: buf.total, Class: d.class})
}

// ------------------------------------------------------------ RP/TP codec

func normalizeNumber(n, cc string) string {
	n = strings.TrimSpace(n)
	if i := strings.IndexByte(n, ','); i >= 0 { // Android "\"+44...\",145"
		n = n[:i]
	}
	n = strings.Trim(n, `"`)
	var b strings.Builder
	for i, r := range n {
		if (r >= '0' && r <= '9') || (r == '+' && i == 0) || r == '*' || r == '#' {
			b.WriteRune(r)
		}
	}
	n = b.String()
	switch {
	case n == "":
		return ""
	case strings.HasPrefix(n, "+"):
		return n
	case strings.HasPrefix(n, "00"):
		return "+" + n[2:]
	case strings.HasPrefix(n, "0") && cc != "":
		return "+" + cc + n[1:]
	}
	return n
}

func bcdDigits(num string) []byte {
	var out []byte
	for i := 0; i < len(num); i += 2 {
		lo := digitNibble(num[i])
		hi := byte(0xF)
		if i+1 < len(num) {
			hi = digitNibble(num[i+1])
		}
		out = append(out, hi<<4|lo)
	}
	return out
}

func digitNibble(c byte) byte {
	switch c {
	case '*':
		return 0xA
	case '#':
		return 0xB
	}
	return c - '0'
}

func digitsFromBCD(b []byte, n int) string {
	var sb strings.Builder
	for _, x := range b {
		for _, d := range []byte{x & 0xF, x >> 4} {
			if sb.Len() >= n || d == 0xF {
				break
			}
			sb.WriteByte("0123456789*#abc"[d])
		}
	}
	return sb.String()
}

func toa(num string) (byte, string) {
	if strings.HasPrefix(num, "+") {
		return 0x91, num[1:]
	}
	return 0x81, num
}

func encodeRPAddress(num string) []byte {
	t, d := toa(num)
	v := append([]byte{t}, bcdDigits(d)...)
	return append([]byte{byte(len(v))}, v...)
}

func decodeRPAddress(b []byte) string {
	if len(b) < 1 {
		return ""
	}
	d := digitsFromBCD(b[1:], 2*len(b[1:]))
	if b[0]&0x70 == 0x10 {
		return "+" + d
	}
	return d
}

func encodeTPAddress(num string) []byte {
	t, d := toa(num)
	return append([]byte{byte(len(d)), t}, bcdDigits(d)...)
}

// buildSubmits encodes text into one or more SMS-SUBMIT TPDUs.
func buildSubmits(dest, text string, srr bool, nextMR func() byte) [][]byte {
	septets, ok := gsm7Encode(text)
	var chunks [][]byte // GSM7: septet chunks; UCS2: octet chunks
	ucs2 := !ok
	if !ucs2 {
		if len(septets) <= 160 {
			chunks = [][]byte{septets}
		} else {
			for len(septets) > 0 {
				n := 153
				if n > len(septets) {
					n = len(septets)
				}
				if septets[n-1] == 0x1B { // never split an escape sequence
					n--
				}
				chunks = append(chunks, septets[:n])
				septets = septets[n:]
			}
		}
	} else {
		u := utf16.Encode([]rune(text))
		if len(u) <= 70 {
			chunks = [][]byte{u16bytes(u)}
		} else {
			for len(u) > 0 {
				n := 67
				if n > len(u) {
					n = len(u)
				}
				if n < len(u) && utf16.IsSurrogate(rune(u[n-1])) && u[n-1] >= 0xD800 && u[n-1] < 0xDC00 {
					n-- // keep surrogate pairs together
				}
				chunks = append(chunks, u16bytes(u[:n]))
				u = u[n:]
			}
		}
	}
	ref := byte(time.Now().UnixNano())
	var out [][]byte
	for i, c := range chunks {
		first := byte(0x01 | 0x10) // SMS-SUBMIT, relative validity period
		if srr {
			first |= 0x20
		}
		var udh []byte
		if len(chunks) > 1 {
			first |= 0x40
			udh = []byte{0x05, 0x00, 0x03, ref, byte(len(chunks)), byte(i + 1)}
		}
		tp := []byte{first, nextMR()}
		tp = append(tp, encodeTPAddress(dest)...)
		dcs := byte(0x00)
		if ucs2 {
			dcs = 0x08
		}
		tp = append(tp, 0x00, dcs, 0xAA) // PID, DCS, VP = 4 days
		if ucs2 {
			ud := append(append([]byte(nil), udh...), c...)
			tp = append(tp, byte(len(ud)))
			tp = append(tp, ud...)
		} else {
			fill := 0
			hdrSeptets := 0
			if udh != nil {
				bits := len(udh) * 8
				hdrSeptets = (bits + 6) / 7
				fill = hdrSeptets*7 - bits
			}
			packed := packSeptets(c, fill)
			tp = append(tp, byte(hdrSeptets+len(c)))
			tp = append(tp, udh...)
			tp = append(tp, packed...)
		}
		out = append(out, tp)
	}
	return out
}

func u16bytes(u []uint16) []byte {
	b := make([]byte, 0, 2*len(u))
	for _, x := range u {
		b = append(b, byte(x>>8), byte(x))
	}
	return b
}

type deliverPDU struct {
	from        string
	text        string
	scts        time.Time
	class       int
	concatRef   int
	concatTotal int
	concatSeq   int
	pid         byte
	// simData marks an SMS-PP data download for the (U)SIM (TP-PID 0x7F or
	// a UDH command-packet header, TS 31.115): OTA data, not user text.
	simData bool
}

func parseDeliver(tp []byte) (deliverPDU, error) {
	var d deliverPDU
	d.class = -1
	first := tp[0]
	udhi := first&0x40 != 0
	b := tp[1:]
	if len(b) < 2 {
		return d, errors.New("short OA")
	}
	nd, t := int(b[0]), b[1]
	ol := (nd + 1) / 2
	if len(b) < 2+ol+9 {
		return d, errors.New("short SMS-DELIVER")
	}
	if t&0x70 == 0x50 { // alphanumeric sender
		d.from = gsm7Decode(unpackSeptets(b[2:2+ol], nd*4/7, 0))
	} else {
		d.from = digitsFromBCD(b[2:2+ol], nd)
		if t&0x70 == 0x10 {
			d.from = "+" + d.from
		}
	}
	b = b[2+ol:]
	d.pid = b[0]
	d.simData = d.pid == 0x7f
	dcs := b[1]
	d.scts = parseSCTS(b[2:9])
	b = b[9:]
	if len(b) < 1 {
		return d, errors.New("missing UDL")
	}
	udl := int(b[0])
	ud := b[1:]
	alphabet := 0 // 0 gsm7, 1 8bit, 2 ucs2
	switch {
	case dcs&0xC0 == 0x00:
		alphabet = int(dcs>>2) & 0x03
		if dcs&0x10 != 0 {
			d.class = int(dcs & 0x03)
		}
	case dcs&0xF0 == 0xF0:
		if dcs&0x04 != 0 {
			alphabet = 1
		}
		d.class = int(dcs & 0x03)
	case dcs&0xF0 == 0xE0:
		alphabet = 2
	}
	hdrOctets := 0
	if udhi && len(ud) > 0 {
		hdrOctets = int(ud[0]) + 1
		if hdrOctets > len(ud) {
			return d, errors.New("bad UDH")
		}
		parseUDH(ud[1:hdrOctets], &d)
	}
	switch alphabet {
	case 2:
		body := ud[hdrOctets:]
		if udl < len(ud) {
			body = ud[hdrOctets:udl]
		}
		var u []uint16
		for i := 0; i+1 < len(body); i += 2 {
			u = append(u, uint16(body[i])<<8|uint16(body[i+1]))
		}
		d.text = string(utf16.Decode(u))
	case 1:
		body := ud[hdrOctets:]
		if udl < len(ud) {
			body = ud[hdrOctets:udl]
		}
		d.text = string(body)
	default:
		skip := 0
		if hdrOctets > 0 {
			skip = (hdrOctets*8 + 6) / 7
		}
		sept := unpackSeptets(ud, udl, 0)
		if skip <= len(sept) {
			sept = sept[skip:]
		}
		d.text = gsm7Decode(sept)
	}
	return d, nil
}

func parseUDH(h []byte, d *deliverPDU) {
	for len(h) >= 2 {
		iei, l := h[0], int(h[1])
		if 2+l > len(h) {
			return
		}
		v := h[2 : 2+l]
		switch {
		case iei == 0x00 && l == 3:
			d.concatRef, d.concatTotal, d.concatSeq = int(v[0]), int(v[1]), int(v[2])
		case iei == 0x08 && l == 4:
			d.concatRef, d.concatTotal, d.concatSeq = int(v[0])<<8|int(v[1]), int(v[2]), int(v[3])
		case iei == 0x70 || iei == 0x71: // (U)SIM toolkit security header (TS 31.115)
			d.simData = true
		}
		h = h[2+l:]
	}
}

func parseSCTS(b []byte) time.Time {
	dig := func(x byte) int { return int(x&0x0F)*10 + int(x>>4) }
	tzq := int(b[6]&0x07)*10 + int(b[6]>>4)
	if b[6]&0x08 != 0 {
		tzq = -tzq
	}
	loc := time.FixedZone("", tzq*15*60)
	return time.Date(2000+dig(b[0]), time.Month(dig(b[1])), dig(b[2]), dig(b[3]), dig(b[4]), dig(b[5]), 0, loc)
}

// ------------------------------------------------------------ GSM 7-bit

const gsm7Basic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\x1bÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"

var (
	gsm7Table  = []rune(gsm7Basic)
	gsm7Ext    = map[byte]rune{0x0A: '\f', 0x14: '^', 0x28: '{', 0x29: '}', 0x2F: '\\', 0x3C: '[', 0x3D: '~', 0x3E: ']', 0x40: '|', 0x65: '€'}
	gsm7Rev    = map[rune]byte{}
	gsm7ExtRev = map[rune]byte{}
)

func init() {
	for i, r := range gsm7Table {
		if i != 0x1B {
			gsm7Rev[r] = byte(i)
		}
	}
	for k, v := range gsm7Ext {
		gsm7ExtRev[v] = k
	}
}

func gsm7Encode(s string) ([]byte, bool) {
	var out []byte
	for _, r := range s {
		if c, ok := gsm7Rev[r]; ok {
			out = append(out, c)
		} else if c, ok := gsm7ExtRev[r]; ok {
			out = append(out, 0x1B, c)
		} else {
			return nil, false
		}
	}
	return out, true
}

func gsm7Decode(sept []byte) string {
	var sb strings.Builder
	for i := 0; i < len(sept); i++ {
		c := sept[i] & 0x7f
		if c == 0x1B && i+1 < len(sept) {
			i++
			if r, ok := gsm7Ext[sept[i]&0x7f]; ok {
				sb.WriteRune(r)
			} else {
				sb.WriteRune(' ')
			}
			continue
		}
		sb.WriteRune(gsm7Table[c])
	}
	return sb.String()
}

// packSeptets packs 7-bit values starting after fill bits.
func packSeptets(s []byte, fill int) []byte {
	total := fill + 7*len(s)
	out := make([]byte, (total+7)/8)
	bit := fill
	for _, c := range s {
		for j := 0; j < 7; j++ {
			if c&(1<<j) != 0 {
				out[bit/8] |= 1 << (bit % 8)
			}
			bit++
		}
	}
	return out
}

// unpackSeptets extracts n septets after fill bits.
func unpackSeptets(b []byte, n, fill int) []byte {
	out := make([]byte, 0, n)
	bit := fill
	for i := 0; i < n; i++ {
		var c byte
		for j := 0; j < 7; j++ {
			if bit/8 >= len(b) {
				return out
			}
			if b[bit/8]&(1<<(bit%8)) != 0 {
				c |= 1 << j
			}
			bit++
		}
		out = append(out, c)
	}
	return out
}
