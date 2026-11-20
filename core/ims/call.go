package ims

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Call states.
const (
	CallDialing  = "dialing"
	CallRinging  = "ringing"  // outgoing, remote alerting
	CallIncoming = "incoming" // incoming, waiting for the user
	CallActive   = "active"
	CallEnded    = "ended"
)

// MediaStats counts received RTP packets and successfully transmitted packets.
type MediaStats struct {
	RxPackets uint64 `json:"rxPackets"`
	TxPackets uint64 `json:"txPackets"`
}

// CallInfo is reported to the application.
type CallInfo struct {
	MediaStats
	ID         string    `json:"id"`
	Number     string    `json:"number"`
	Incoming   bool      `json:"incoming"`
	State      string    `json:"state"`
	Codec      string    `json:"codec,omitempty"`
	MaxMode    int       `json:"maxMode,omitempty"`
	OctetAlign bool      `json:"octetAlign,omitempty"`
	EarlyMedia bool      `json:"earlyMedia,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	SIPCode    int       `json:"sipCode,omitempty"`
	Started    time.Time `json:"started"`
	Answered   time.Time `json:"answered,omitempty"`
}

type dialog struct {
	mu           sync.Mutex
	info         CallInfo
	callID       string
	localTag     string
	remoteTag    string
	localURI     string // From (outgoing) / To (incoming) value without tag
	remoteURI    string
	remoteTarget string
	routes       []string
	cseq         int
	invite       *Message // UAC: our INVITE; UAS: received INVITE
	rtp          *rtpSession
	sdp          *sdpBuilder
	remoteSDP    *sdpMedia
	codec        Codec
	answerSent   bool // UAS: SDP answer already sent reliably
	rseq         int
	prack        chan struct{}
	ended        chan struct{}
	endOnce      sync.Once
	cancelDial   context.CancelFunc
}

type callState struct {
	ua    *UA
	mu    sync.Mutex
	calls map[string]*dialog // by Call-ID
	// Media hooks set by the application.
	OnAudio func(callID string, f AudioFrame)
	OnDTMF  func(callID string, d rune)
}

func newCallState(ua *UA) *callState { return &callState{ua: ua, calls: map[string]*dialog{}} }

func (cs *callState) get(id string) *dialog {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.calls[id]
}

func (d *dialog) snapshot() CallInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	info := d.info
	if d.rtp != nil {
		info.MediaStats = d.rtp.stats()
	}
	return info
}

func (cs *callState) emit(d *dialog) {
	cs.ua.emit("call", d.snapshot())
}

// CallMediaStats returns counters without copying subscriber identities.
func (ua *UA) CallMediaStats(callID string) (MediaStats, error) {
	d := ua.calls.get(callID)
	if d == nil {
		return MediaStats{}, errors.New("ims: no such call")
	}
	return d.snapshot().MediaStats, nil
}

func (cs *callState) end(d *dialog, reason string, code int) {
	d.endOnce.Do(func() {
		d.mu.Lock()
		d.info.State = CallEnded
		d.info.Reason = reason
		if code != 0 {
			d.info.SIPCode = code
		}
		rtp := d.rtp
		d.mu.Unlock()
		if rtp != nil {
			rtp.close()
		}
		close(d.ended)
		cs.mu.Lock()
		delete(cs.calls, d.callID)
		cs.mu.Unlock()
		cs.emit(d)
	})
}

func (cs *callState) hangupAll() {
	cs.mu.Lock()
	var ids []string
	for id := range cs.calls {
		ids = append(ids, id)
	}
	cs.mu.Unlock()
	for _, id := range ids {
		_ = cs.ua.Hangup(context.Background(), id)
	}
}

func (cs *callState) attachMedia(d *dialog) {
	id := d.callID
	d.rtp.mu.Lock()
	d.rtp.onRecv = func(f AudioFrame) {
		if h := cs.OnAudio; h != nil {
			h(id, f)
		}
	}
	d.rtp.onDTMF = func(r rune) {
		if h := cs.OnDTMF; h != nil {
			h(id, r)
		}
	}
	d.rtp.mu.Unlock()
}

// SetMediaHandlers installs callbacks for received audio and DTMF.
func (ua *UA) SetMediaHandlers(audio func(string, AudioFrame), dtmf func(string, rune)) {
	ua.calls.OnAudio = audio
	ua.calls.OnDTMF = dtmf
}

// SendAudio sends one 20 ms AMR storage-format frame on a call.
func (ua *UA) SendAudio(callID string, frame []byte) error {
	d := ua.calls.get(callID)
	if d == nil {
		return errors.New("ims: no such call")
	}
	if len(frame) < 1 {
		return nil
	}
	return d.rtp.sendFrame(frame)
}

// SendDTMF sends a DTMF digit on an active call.
func (ua *UA) SendDTMF(callID string, digit rune) error {
	d := ua.calls.get(callID)
	if d == nil {
		return errors.New("ims: no such call")
	}
	return d.rtp.sendDTMF(digit)
}

// Calls lists current calls.
func (ua *UA) Calls() []CallInfo {
	ua.calls.mu.Lock()
	defer ua.calls.mu.Unlock()
	var out []CallInfo
	for _, d := range ua.calls.calls {
		out = append(out, d.snapshot())
	}
	return out
}

// dialURI builds the Request-URI for a number.
func (ua *UA) dialURI(num string) string {
	if ua.cfg.DialURIScheme == "sip" || !strings.HasPrefix(num, "+") {
		u := "sip:" + num + "@" + ua.cfg.Domain
		if ua.cfg.UserEqPhone || strings.HasPrefix(num, "+") {
			u += ";user=phone"
		}
		return u
	}
	return "tel:" + num
}

func (ua *UA) callContact() string {
	c := fmt.Sprintf("<sip:%s>", ua.tr.HostPort(ua.tr.PortS))
	c += `;+g.3gpp.icsi-ref="urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel";audio`
	if ua.instance != "" {
		c += `;+sip.instance="` + ua.instance + `"`
	}
	return c
}

// ------------------------------------------------------------ outgoing

// Dial starts an outgoing call and returns its ID immediately; progress is
// reported through "call" events.
func (ua *UA) Dial(ctx context.Context, number string) (CallInfo, error) {
	num := normalizeNumber(number, ua.cfg.CountryCode)
	if num == "" {
		return CallInfo{}, errors.New("ims: invalid number")
	}
	rtp, err := newRTPSession(ua)
	if err != nil {
		return CallInfo{}, err
	}
	target := ua.dialURI(num)
	pid := ua.preferredIdentity()
	d := &dialog{callID: randHex(12) + "@" + ua.tr.Local.String(), localTag: newTag(), cseq: 1,
		localURI: "<" + pid + ">", remoteURI: "<" + target + ">", remoteTarget: target,
		rtp: rtp, sdp: newSDPBuilder(), prack: make(chan struct{}, 1), ended: make(chan struct{}),
		info: CallInfo{Number: num, State: CallDialing, Started: time.Now()}}
	d.info.ID = d.callID
	ua.calls.mu.Lock()
	ua.calls.calls[d.callID] = d
	ua.calls.mu.Unlock()
	ua.calls.attachMedia(d)

	inv := &Message{Method: "INVITE", RequestURI: target}
	inv.Add("Max-Forwards", "70")
	ua.addRoute(inv, true)
	inv.Add("From", d.localURI+";tag="+d.localTag)
	inv.Add("To", d.remoteURI)
	inv.Add("Call-ID", d.callID)
	inv.Add("CSeq", "1 INVITE")
	inv.Add("Contact", ua.callContact())
	inv.Add("P-Preferred-Identity", "<"+pid+">")
	inv.Add("P-Preferred-Service", "urn:urn-7:3gpp-service.ims.icsi.mmtel")
	inv.Add("Accept-Contact", `*;+g.3gpp.icsi-ref="urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel"`)
	inv.Add("P-Access-Network-Info", ua.cfg.PANI)
	inv.Add("Supported", "100rel, timer, replaces")
	inv.Add("Session-Expires", "1800")
	inv.Add("Min-SE", "90")
	inv.Add("Allow", "INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, MESSAGE, NOTIFY, OPTIONS, REFER, INFO")
	inv.Add("User-Agent", ua.cfg.UserAgent)
	ua.addSecurity(inv)
	inv.Add("Content-Type", "application/sdp")
	inv.Body = d.sdp.offer(ua.tr.Local, rtp.port)
	d.invite = inv

	dctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	d.cancelDial = cancel
	ua.calls.emit(d)
	go ua.runOutgoing(dctx, d, inv)
	return d.info, nil
}

func (ua *UA) runOutgoing(ctx context.Context, d *dialog, inv *Message) {
	defer d.cancelDial()
	resp, err := ua.tr.Request(ctx, inv, func(p *Message) { ua.onProvisional(d, p) })
	if err != nil {
		ua.calls.end(d, "network error: "+err.Error(), 0)
		return
	}
	if resp.StatusCode >= 300 {
		ua.ackNon2xx(inv, resp)
		// Record what the network said: Reason/Warning explain most failures.
		ua.log.Info("call: INVITE rejected", "code", resp.StatusCode, "phrase", resp.Reason,
			"reason", resp.Get("Reason"), "warning", resp.Get("Warning"))
		reason := resp.Reason
		if r := resp.Get("Reason"); r != "" {
			if t, ok := Param(r, "text"); ok {
				reason = t
			}
		}
		ua.calls.end(d, reason, resp.StatusCode)
		return
	}
	ua.updateDialogFromResponse(d, resp)
	if len(resp.Body) > 0 {
		if err := ua.applyRemoteSDP(d, resp.Body); err != nil {
			ua.log.Warn("call: bad SDP in 200", "err", err)
		}
	}
	ua.sendInDialogACK(d)
	d.mu.Lock()
	d.info.State = CallActive
	d.info.Answered = time.Now()
	d.mu.Unlock()
	ua.calls.emit(d)
	ua.watchMedia(d)
}

func (ua *UA) updateDialogFromResponse(d *dialog, r *Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if tag, ok := Param(r.Get("To"), "tag"); ok && d.remoteTag == "" {
		d.remoteTag = tag
	}
	if c := r.Get("Contact"); c != "" {
		d.remoteTarget = URIOf(c)
	}
	if rr := r.Values("Record-Route"); len(rr) > 0 && d.routes == nil {
		for i := len(rr) - 1; i >= 0; i-- {
			d.routes = append(d.routes, rr[i])
		}
	}
}

func (ua *UA) onProvisional(d *dialog, p *Message) {
	if p.StatusCode == 100 {
		return
	}
	ua.log.Info("call: provisional", "code", p.StatusCode, "phrase", p.Reason,
		"sdp", len(p.Body) > 0, "100rel", strings.Contains(p.Get("Require"), "100rel"))
	ua.updateDialogFromResponse(d, p)
	if len(p.Body) > 0 && strings.Contains(strings.ToLower(p.Get("Content-Type")), "sdp") {
		if err := ua.applyRemoteSDP(d, p.Body); err == nil {
			d.mu.Lock()
			d.info.EarlyMedia = true
			d.mu.Unlock()
		}
	}
	if strings.Contains(p.Get("Require"), "100rel") {
		if rs, err := strconv.Atoi(strings.TrimSpace(p.Get("RSeq"))); err == nil {
			go ua.sendPRACK(d, rs, p.Get("CSeq"))
		}
	}
	d.mu.Lock()
	changed := false
	if (p.StatusCode == 180 || p.StatusCode == 183) && d.info.State == CallDialing {
		d.info.State = CallRinging
		changed = true
	}
	d.mu.Unlock()
	if changed || p.StatusCode == 183 {
		ua.calls.emit(d)
	}
}

func (ua *UA) applyRemoteSDP(d *dialog, body []byte) error {
	m, err := parseSDP(body)
	if err != nil {
		return err
	}
	c, err := m.selectCodec()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.remoteSDP, d.codec = m, c
	d.info.Codec, d.info.MaxMode, d.info.OctetAlign = c.Name, c.MaxMode(), c.OctetAlign
	d.mu.Unlock()
	d.rtp.configure(m, c)
	return nil
}

// newInDialog builds a request within the dialog.
func (ua *UA) newInDialog(d *dialog, method string) *Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	if method != "ACK" {
		d.cseq++
	}
	m := &Message{Method: method, RequestURI: d.remoteTarget}
	m.Add("Max-Forwards", "70")
	for _, r := range d.routes {
		m.Add("Route", r)
	}
	incoming := d.info.Incoming
	from, to := d.localURI+";tag="+d.localTag, d.remoteURI
	if d.remoteTag != "" {
		to += ";tag=" + d.remoteTag
	}
	_ = incoming
	m.Add("From", from)
	m.Add("To", to)
	m.Add("Call-ID", d.callID)
	cs := d.cseq
	if method == "ACK" {
		cs = 1
		if d.invite != nil {
			if n, err := strconv.Atoi(strings.Fields(d.invite.Get("CSeq"))[0]); err == nil {
				cs = n
			}
		}
	}
	m.Add("CSeq", fmt.Sprintf("%d %s", cs, method))
	m.Add("P-Access-Network-Info", ua.cfg.PANI)
	m.Add("User-Agent", ua.cfg.UserAgent)
	return m
}

// newInDialogSecured adds sec-agree headers (not for ACK).
func (ua *UA) newInDialogSecured(d *dialog, method string) *Message {
	m := ua.newInDialog(d, method)
	ua.addSecurity(m)
	return m
}

func (ua *UA) sendPRACK(d *dialog, rseq int, cseq string) {
	m := ua.newInDialogSecured(d, "PRACK")
	m.Add("RAck", fmt.Sprintf("%d %s", rseq, cseq))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := ua.tr.Request(ctx, m, nil); err != nil {
		ua.log.Warn("call: PRACK failed", "err", err)
	}
}

func (ua *UA) sendInDialogACK(d *dialog) {
	ack := ua.newInDialog(d, "ACK")
	if err := ua.tr.SendACK(ack); err != nil {
		ua.log.Warn("call: ACK failed", "err", err)
	}
}

// ackNon2xx acknowledges a failure response inside the INVITE transaction.
func (ua *UA) ackNon2xx(inv, resp *Message) {
	ack := &Message{Method: "ACK", RequestURI: inv.RequestURI}
	ack.Add("Via", inv.Get("Via"))
	ack.Add("Max-Forwards", "70")
	for _, r := range inv.Values("Route") {
		ack.Add("Route", r)
	}
	ack.Add("From", inv.Get("From"))
	ack.Add("To", resp.Get("To"))
	ack.Add("Call-ID", inv.Get("Call-ID"))
	ack.Add("CSeq", strings.Fields(inv.Get("CSeq"))[0]+" ACK")
	_ = ua.tr.SendRaw(ack)
}

// Hangup ends, cancels or rejects a call.
func (ua *UA) Hangup(ctx context.Context, callID string) error {
	d := ua.calls.get(callID)
	if d == nil {
		return errors.New("ims: no such call")
	}
	d.mu.Lock()
	state, incoming := d.info.State, d.info.Incoming
	inv := d.invite
	d.mu.Unlock()
	switch {
	case incoming && state == CallIncoming:
		r := NewResponse(inv, 603, "Decline")
		r.Set("To", d.toWithTag(inv))
		_ = ua.tr.Reply(inv, r)
		ua.calls.end(d, "declined", 603)
	case !incoming && (state == CallDialing || state == CallRinging):
		c := &Message{Method: "CANCEL", RequestURI: inv.RequestURI}
		c.Add("Via", inv.Get("Via"))
		c.Add("Max-Forwards", "70")
		for _, r := range inv.Values("Route") {
			c.Add("Route", r)
		}
		c.Add("From", inv.Get("From"))
		c.Add("To", inv.Get("To"))
		c.Add("Call-ID", inv.Get("Call-ID"))
		c.Add("CSeq", strings.Fields(inv.Get("CSeq"))[0]+" CANCEL")
		_ = ua.tr.SendRaw(c)
		ua.calls.end(d, "cancelled", 487)
		// The INVITE transaction will complete with 487; give it time.
		go func() {
			select {
			case <-time.After(5 * time.Second):
				d.cancelDial()
			case <-ctx.Done():
			}
		}()
	default:
		bye := ua.newInDialogSecured(d, "BYE")
		ua.calls.end(d, "hung up", 0)
		bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := ua.tr.Request(bctx, bye, nil)
		return err
	}
	return nil
}

func (d *dialog) toWithTag(req *Message) string {
	to := req.Get("To")
	if _, ok := Param(to, "tag"); !ok {
		to += ";tag=" + d.localTag
	}
	return to
}

// watchMedia ends a call if RTP stops arriving for a long time.
func (ua *UA) watchMedia(d *dialog) {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-d.ended:
				return
			case <-t.C:
				last := d.rtp.lastRx.Load()
				if last != 0 && time.Since(time.Unix(0, last)) > 60*time.Second {
					ua.log.Warn("call: no RTP for 60s, hanging up")
					_ = ua.Hangup(context.Background(), d.callID)
					return
				}
			}
		}
	}()
}

// ------------------------------------------------------------ incoming

func (cs *callState) handleRequest(m *Message) {
	ua := cs.ua
	d := cs.get(m.Get("Call-ID"))
	switch m.Method {
	case "INVITE":
		if d != nil {
			ua.handleReInvite(d, m)
			return
		}
		ua.handleNewInvite(m)
	case "ACK":
		if d != nil {
			d.mu.Lock()
			if d.info.Incoming && d.info.State != CallActive && d.info.State != CallEnded && !d.info.Answered.IsZero() {
				d.info.State = CallActive
			}
			d.mu.Unlock()
			cs.emit(d)
		}
	case "PRACK":
		_ = ua.tr.Reply(m, NewResponse(m, 200, "OK"))
		if d != nil {
			select {
			case d.prack <- struct{}{}:
			default:
			}
		}
	case "UPDATE":
		if d == nil {
			_ = ua.tr.Reply(m, NewResponse(m, 481, "Call/Transaction Does Not Exist"))
			return
		}
		ua.answerOffer(d, m, true)
	case "BYE":
		ua.log.Info("call: BYE from network", "reason", m.Get("Reason"))
		_ = ua.tr.Reply(m, NewResponse(m, 200, "OK"))
		if d != nil {
			cs.end(d, "remote hung up", 0)
		}
	case "CANCEL":
		ua.log.Info("call: CANCEL from network", "reason", m.Get("Reason"))
		_ = ua.tr.Reply(m, NewResponse(m, 200, "OK"))
		if d != nil && d.info.Incoming {
			r := NewResponse(d.invite, 487, "Request Terminated")
			r.Set("To", d.toWithTag(d.invite))
			_ = ua.tr.Reply(d.invite, r)
			cs.end(d, "missed", 487)
		}
	case "INFO":
		_ = ua.tr.Reply(m, NewResponse(m, 200, "OK"))
	default:
		_ = ua.tr.Reply(m, NewResponse(m, 405, "Method Not Allowed"))
	}
}

func (ua *UA) handleNewInvite(m *Message) {
	_ = ua.tr.Reply(m, NewResponse(m, 100, "Trying"))
	remote, err := parseSDP(m.Body)
	if err != nil {
		_ = ua.tr.Reply(m, NewResponse(m, 488, "Not Acceptable Here"))
		return
	}
	codec, err := remote.selectCodec()
	if err != nil {
		_ = ua.tr.Reply(m, NewResponse(m, 488, "Not Acceptable Here"))
		return
	}
	rtp, err := newRTPSession(ua)
	if err != nil {
		_ = ua.tr.Reply(m, NewResponse(m, 500, "Server Internal Error"))
		return
	}
	from := m.Get("From")
	caller := NumberFromURI(from)
	if pai := m.Values("P-Asserted-Identity"); len(pai) > 0 {
		for _, p := range pai {
			if n := NumberFromURI(p); n != "" {
				caller = n
				break
			}
		}
	}
	tag, _ := Param(from, "tag")
	d := &dialog{callID: m.Get("Call-ID"), localTag: newTag(), remoteTag: tag, cseq: 0,
		localURI: strings.Split(m.Get("To"), ";tag=")[0], remoteURI: strings.Split(from, ";tag=")[0],
		remoteTarget: URIOf(m.Get("Contact")), invite: m, rtp: rtp, sdp: newSDPBuilder(),
		remoteSDP: remote, codec: codec, prack: make(chan struct{}, 1), ended: make(chan struct{}),
		info: CallInfo{Number: caller, Incoming: true, State: CallIncoming, Started: time.Now(),
			Codec: codec.Name, MaxMode: codec.MaxMode(), OctetAlign: codec.OctetAlign}}
	d.info.ID = d.callID
	for _, r := range m.Values("Record-Route") {
		d.routes = append(d.routes, r)
	}
	rtp.configure(remote, codec)
	ua.calls.mu.Lock()
	ua.calls.calls[d.callID] = d
	ua.calls.mu.Unlock()
	ua.calls.attachMedia(d)

	reliable := strings.Contains(m.Get("Require"), "100rel") ||
		(strings.Contains(m.Get("Supported"), "100rel") && remote.precond)
	if remote.precond || strings.Contains(m.Get("Require"), "precondition") {
		// 183 with the SDP answer, reliably, then wait for PRACK and the
		// UPDATE confirming the remote QoS before alerting.
		r := ua.provisional(d, 183, "Session Progress", reliable)
		r.Add("Content-Type", "application/sdp")
		r.Body = d.sdp.answer(ua.tr.Local, rtp.port, remote, codec, false)
		d.answerSent = reliable
		_ = ua.tr.Reply(m, r)
		if reliable {
			select {
			case <-d.prack:
			case <-time.After(8 * time.Second):
			}
		}
		// The UPDATE (if any) is answered by handleRequest; give it a moment.
		time.Sleep(300 * time.Millisecond)
	}
	r := ua.provisional(d, 180, "Ringing", reliable && strings.Contains(m.Get("Require"), "100rel"))
	_ = ua.tr.Reply(m, r)
	ua.calls.emit(d)
	// Auto-reject if the user does not answer.
	go func() {
		select {
		case <-d.ended:
		case <-time.After(90 * time.Second):
			d.mu.Lock()
			st := d.info.State
			d.mu.Unlock()
			if st == CallIncoming {
				rr := NewResponse(m, 480, "Temporarily Unavailable")
				rr.Set("To", d.toWithTag(m))
				_ = ua.tr.Reply(m, rr)
				ua.calls.end(d, "missed", 480)
			}
		}
	}()
}

func (ua *UA) provisional(d *dialog, code int, reason string, reliable bool) *Message {
	r := NewResponse(d.invite, code, reason)
	r.Set("To", d.toWithTag(d.invite))
	r.Add("Contact", ua.callContact())
	r.Add("Allow", "INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, MESSAGE, NOTIFY, OPTIONS, REFER, INFO")
	if reliable {
		d.mu.Lock()
		d.rseq++
		rs := d.rseq
		d.mu.Unlock()
		r.Add("Require", "100rel")
		r.Add("RSeq", strconv.Itoa(rs))
	}
	return r
}

// Answer accepts an incoming call.
func (ua *UA) Answer(callID string) error {
	d := ua.calls.get(callID)
	if d == nil {
		return errors.New("ims: no such call")
	}
	d.mu.Lock()
	if d.info.State != CallIncoming {
		d.mu.Unlock()
		return errors.New("ims: call is not ringing")
	}
	d.info.Answered = time.Now()
	inv, sent := d.invite, d.answerSent
	d.mu.Unlock()
	r := NewResponse(inv, 200, "OK")
	r.Set("To", d.toWithTag(inv))
	r.Add("Contact", ua.callContact())
	r.Add("Allow", "INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, MESSAGE, NOTIFY, OPTIONS, REFER, INFO")
	r.Add("Supported", "100rel, timer")
	if !sent {
		r.Add("Content-Type", "application/sdp")
		r.Body = d.sdp.answer(ua.tr.Local, d.rtp.port, d.remoteSDP, d.codec, true)
	}
	if err := ua.tr.Reply(inv, r); err != nil {
		return err
	}
	d.mu.Lock()
	d.info.State = CallActive
	d.mu.Unlock()
	ua.calls.emit(d)
	ua.watchMedia(d)
	return nil
}

// answerOffer handles UPDATE / re-INVITE carrying (or not) an SDP offer.
func (ua *UA) answerOffer(d *dialog, m *Message, update bool) {
	r := NewResponse(m, 200, "OK")
	r.Add("Contact", ua.callContact())
	if len(m.Body) > 0 {
		remote, err := parseSDP(m.Body)
		if err == nil {
			c, err2 := remote.selectCodec()
			if err2 != nil {
				c = d.codec
			}
			d.mu.Lock()
			d.remoteSDP, d.codec = remote, c
			d.mu.Unlock()
			d.rtp.configure(remote, c)
			r.Add("Content-Type", "application/sdp")
			r.Body = d.sdp.answer(ua.tr.Local, d.rtp.port, remote, c, true)
		}
	}
	_ = ua.tr.Reply(m, r)
}

func (ua *UA) handleReInvite(d *dialog, m *Message) {
	_ = ua.tr.Reply(m, NewResponse(m, 100, "Trying"))
	if len(m.Body) == 0 {
		// Offer-less re-INVITE: we offer in the 200, the peer answers in ACK.
		r := NewResponse(m, 200, "OK")
		r.Add("Contact", ua.callContact())
		r.Add("Content-Type", "application/sdp")
		r.Body = d.sdp.offer(ua.tr.Local, d.rtp.port)
		_ = ua.tr.Reply(m, r)
		return
	}
	ua.answerOffer(d, m, false)
}
