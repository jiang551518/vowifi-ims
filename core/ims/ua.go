package ims

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"vowifi/ike"
	"vowifi/netio"
)

// AKAFunc runs AKA on the SIM (same contract as ike.AKAFunc).
type AKAFunc = ike.AKAFunc

// Config describes the subscriber and the access.
type Config struct {
	IMSI, MCC, MNC string
	IMPI           string   // from ISIM; derived from IMSI when empty
	Domain         string   // home network domain; derived when empty
	IMPU           []string // from ISIM; temporary IMPU derived when empty
	IMEI           string
	// InstanceID, when set, is used as +sip.instance (urn:uuid) instead of the
	// IMEI. Not used by the app: EE (CTExcel) accepts a UUID registration but
	// ends every call from it with 487 within a second (verified 2026-11-20:
	// the same SIM/network connects with the IMEI-based instance). The app
	// keeps the IMEI instance and avoids clashing with the phone's own VoLTE
	// by taking that SIM off cellular instead. Kept for testing (vwcli).
	InstanceID string
	AKA        AKAFunc
	PCSCF      []net.IP
	Stack      *netio.Stack
	SecAgree   bool // use IPsec sec-agree (recommended)
	PreferTCP  bool // SIP over TCP
	UserAgent  string
	PANI       string // P-Access-Network-Info
	Expires    int
	Logger     *slog.Logger
	OnEvent    func(Event)
	// Carrier quirks.
	DialURIScheme string // "tel" (default) or "sip"
	UserEqPhone   bool   // add ;user=phone to sip: dial URIs
	SMSCAddress   string // international SC address from the SIM (e.g. +447...)
	CountryCode   string // E.164 country code used to normalise local numbers
}

// Event is reported to the application.
type Event struct {
	Kind string `json:"kind"` // registration | sms | sms_status | call
	Data any    `json:"data"`
}

// Registration describes the IMS registration state.
type Registration struct {
	Registered bool     `json:"registered"`
	URIs       []string `json:"uris,omitempty"`
	Number     string   `json:"number,omitempty"`
	Expires    int      `json:"expires,omitempty"`
	Transport  string   `json:"transport,omitempty"`
	Security   string   `json:"security,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// UA is a registered IMS user agent.
type UA struct {
	cfg  Config
	log  *slog.Logger
	tr   *Transport
	impu string

	mu           sync.Mutex
	callID       string
	cseq         int
	fromTag      string
	serviceRoute []string
	assocURIs    []string
	expires      int
	nonceCount   int
	lastChal     map[string]string
	lastRES      []byte
	secMech      string
	secVerify    []string // Security-Server values echoed in later requests
	registered   bool
	contact      string
	instance     string

	stop     chan struct{}
	done     chan struct{}
	doneOnce sync.Once
	stopped  sync.Once
	sms      *smsState
	calls    *callState
}

func (c *Config) defaults() {
	mnc := c.MNC
	if len(mnc) == 2 {
		mnc = "0" + mnc
	}
	if c.Domain == "" {
		c.Domain = "ims.mnc" + mnc + ".mcc" + c.MCC + ".3gppnetwork.org"
	}
	if c.IMPI == "" {
		c.IMPI = c.IMSI + "@" + c.Domain
	}
	if c.Expires == 0 {
		c.Expires = 600000
	}
	if c.UserAgent == "" {
		c.UserAgent = "VoWiFi-Android/1.0"
	}
	if c.PANI == "" {
		c.PANI = "IEEE-802.11;i-wlan-node-id=000000000000"
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.DialURIScheme == "" {
		c.DialURIScheme = "tel"
	}
}

func randomPort() int {
	// Even port in [30000, 59998]; port+1 is used as the server port.
	n, _ := rand.Int(rand.Reader, big.NewInt(15000))
	return 30000 + int(n.Int64())*2
}

func randomSPI() uint32 {
	// Keep SPIs below 2^31: some P-CSCFs parse them as signed integers.
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])&0x7fffffff | 0x10000000
}

// Start registers with the IMS network.
func Start(ctx context.Context, cfg Config) (*UA, error) {
	cfg.defaults()
	if len(cfg.PCSCF) == 0 {
		return nil, errors.New("ims: no P-CSCF address from the ePDG")
	}
	var lastErr error
	for _, p := range cfg.PCSCF {
		ua, err := start(ctx, cfg, p)
		if err == nil {
			return ua, nil
		}
		lastErr = err
		cfg.Logger.Warn("ims: registration via P-CSCF failed", "pcscf", p, "err", err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func start(ctx context.Context, cfg Config, pcscf net.IP) (*UA, error) {
	portC := randomPort()
	tr, err := NewTransport(cfg.Stack, pcscf, portC, portC+1, cfg.Logger)
	if err != nil {
		return nil, err
	}
	tr.PreferTCP = cfg.PreferTCP
	ua := &UA{cfg: cfg, log: cfg.Logger, tr: tr, stop: make(chan struct{}), done: make(chan struct{}),
		callID: randHex(12) + "@" + tr.Local.String(), fromTag: newTag()}
	ua.impu = "sip:" + cfg.IMSI + "@" + cfg.Domain
	if len(cfg.IMPU) > 0 && cfg.IMPU[0] != "" {
		ua.impu = cfg.IMPU[0]
	}
	switch {
	case cfg.InstanceID != "":
		ua.instance = "<urn:uuid:" + cfg.InstanceID + ">"
	case len(cfg.IMEI) >= 14:
		d := cfg.IMEI
		ua.instance = fmt.Sprintf("<urn:gsma:imei:%s-%s-0>", d[:8], d[8:14])
	}
	ua.sms = newSMSState(ua)
	ua.calls = newCallState(ua)
	tr.SetHandler(ua.handleRequest)
	if err := ua.register(ctx, true); err != nil {
		tr.Close()
		return nil, err
	}
	go ua.refreshLoop()
	return ua, nil
}

func (ua *UA) emit(kind string, data any) {
	if ua.cfg.OnEvent != nil {
		ua.cfg.OnEvent(Event{Kind: kind, Data: data})
	}
}

// ProxyAddress is the P-CSCF selected for this registration.
func (ua *UA) ProxyAddress() string { return ua.tr.PCSCF.String() }

// Registration returns the current state.
func (ua *UA) Registration() Registration {
	ua.mu.Lock()
	defer ua.mu.Unlock()
	proto := "UDP"
	if ua.tr.PreferTCP {
		proto = "TCP"
	}
	return Registration{Registered: ua.registered, URIs: append([]string(nil), ua.assocURIs...),
		Number: ua.ownNumber(), Expires: ua.expires, Transport: proto, Security: ua.secMech}
}

// ownNumber picks the MSISDN from P-Associated-URI.
func (ua *UA) ownNumber() string {
	for _, u := range ua.assocURIs {
		if n := NumberFromURI(u); strings.HasPrefix(n, "+") {
			return n
		}
	}
	return ""
}

// preferredIdentity is used in P-Preferred-Identity / From of requests.
func (ua *UA) preferredIdentity() string {
	ua.mu.Lock()
	defer ua.mu.Unlock()
	for _, u := range ua.assocURIs {
		if strings.HasPrefix(URIOf(u), "sip:+") {
			return URIOf(u)
		}
	}
	for _, u := range ua.assocURIs {
		if strings.HasPrefix(URIOf(u), "tel:") {
			return URIOf(u)
		}
	}
	return ua.impu
}

// ------------------------------------------------------------ REGISTER

func (ua *UA) contactHeader(expires int) string {
	c := fmt.Sprintf("<sip:%s>", ua.tr.HostPort(ua.tr.PortS))
	c += `;+g.3gpp.icsi-ref="urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel";+g.3gpp.smsip;audio`
	if ua.instance != "" {
		c += `;+sip.instance="` + ua.instance + `"`
	}
	if expires >= 0 {
		c += ";expires=" + strconv.Itoa(expires)
	}
	return c
}

type secOffer struct {
	spiC, spiS uint32
}

func (ua *UA) securityClient(o secOffer) []string {
	var out []string
	for _, m := range []struct{ alg, ealg string }{
		{"hmac-sha-1-96", "aes-cbc"}, {"hmac-sha-1-96", "null"},
		{"hmac-md5-96", "aes-cbc"}, {"hmac-md5-96", "null"},
	} {
		out = append(out, fmt.Sprintf("ipsec-3gpp;alg=%s;ealg=%s;spi-c=%d;spi-s=%d;port-c=%d;port-s=%d;prot=esp;mod=trans",
			m.alg, m.ealg, o.spiC, o.spiS, ua.tr.PortC, ua.tr.PortS))
	}
	return out
}

func (ua *UA) newRegister(expires int, offer *secOffer) *Message {
	ua.mu.Lock()
	ua.cseq++
	cseq := ua.cseq
	ua.mu.Unlock()
	m := &Message{Method: "REGISTER", RequestURI: "sip:" + ua.cfg.Domain}
	m.Add("Max-Forwards", "70")
	m.Add("From", "<"+ua.impu+">;tag="+ua.fromTag)
	m.Add("To", "<"+ua.impu+">")
	m.Add("Call-ID", ua.callID)
	m.Add("CSeq", strconv.Itoa(cseq)+" REGISTER")
	m.Add("Contact", ua.contactHeader(-1))
	m.Add("Expires", strconv.Itoa(expires))
	m.Add("Supported", "path, gruu, sec-agree")
	if offer != nil {
		m.Add("Require", "sec-agree")
		m.Add("Proxy-Require", "sec-agree")
		for _, v := range ua.securityClient(*offer) {
			m.Add("Security-Client", v)
		}
	}
	m.Add("Allow", "INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, MESSAGE, NOTIFY, OPTIONS, REFER, INFO")
	m.Add("P-Access-Network-Info", ua.cfg.PANI)
	m.Add("User-Agent", ua.cfg.UserAgent)
	return m
}

func (ua *UA) authHeader(chal map[string]string, uri string, res []byte, auts []byte) string {
	if chal == nil {
		return fmt.Sprintf(`Digest username="%s",realm="%s",uri="%s",nonce="",response="",algorithm=AKAv1-MD5`,
			ua.cfg.IMPI, ua.cfg.Domain, uri)
	}
	ua.mu.Lock()
	ua.nonceCount++
	nc := fmt.Sprintf("%08x", ua.nonceCount)
	ua.mu.Unlock()
	realm, nonce := chal["realm"], chal["nonce"]
	cnonce := randHex(8)
	ha1 := md5hex(ua.cfg.IMPI + ":" + realm + ":" + string(res))
	ha2 := md5hex("REGISTER:" + uri)
	qop := ""
	if q := chal["qop"]; q != "" {
		for _, o := range strings.Split(q, ",") {
			if strings.TrimSpace(o) == "auth" {
				qop = "auth"
			}
		}
	}
	var resp string
	if qop != "" {
		resp = md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	} else {
		resp = md5hex(ha1 + ":" + nonce + ":" + ha2)
	}
	h := fmt.Sprintf(`Digest username="%s",realm="%s",uri="%s",nonce="%s",response="%s",algorithm=%s`,
		ua.cfg.IMPI, realm, uri, nonce, resp, chal["algorithm"])
	if qop != "" {
		h += fmt.Sprintf(`,qop=%s,nc=%s,cnonce="%s"`, qop, nc, cnonce)
	}
	if op := chal["opaque"]; op != "" {
		h += `,opaque="` + op + `"`
	}
	if auts != nil {
		h += `,auts="` + base64.StdEncoding.EncodeToString(auts) + `"`
	}
	return h
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// register performs (re-)registration including the AKA challenge.
func (ua *UA) register(ctx context.Context, initial bool) error {
	offer := &secOffer{spiC: randomSPI(), spiS: randomSPI()}
	if !ua.cfg.SecAgree {
		offer = nil
	}
	uri := "sip:" + ua.cfg.Domain
	req := ua.newRegister(ua.cfg.Expires, offer)
	ua.mu.Lock()
	chal, res := ua.lastChal, ua.lastRES
	ua.mu.Unlock()
	if initial || chal == nil {
		req.Add("Authorization", ua.authHeader(nil, uri, nil, nil))
	} else {
		req.Add("Authorization", ua.authHeader(chal, uri, res, nil))
		ua.addRoute(req, false)
	}
	resp, err := ua.tr.Request(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("ims: REGISTER: %w", err)
	}
	challenges, intervalRetries := 0, 0
	for {
		if resp.StatusCode == 401 && challenges < 2 {
			challenges++
			resp, err = ua.answerChallenge(ctx, resp, offer, uri)
		} else if resp.StatusCode == 423 && intervalRetries < 2 {
			minimum, parseErr := strconv.Atoi(strings.TrimSpace(resp.Get("Min-Expires")))
			if parseErr != nil || minimum <= ua.cfg.Expires {
				return errors.New("ims: invalid or non-increasing Min-Expires")
			}
			intervalRetries++
			ua.cfg.Expires = minimum
			req = ua.newRegister(minimum, offer)
			ua.mu.Lock()
			chal, res = ua.lastChal, ua.lastRES
			verify := append([]string(nil), ua.secVerify...)
			ua.mu.Unlock()
			for _, v := range verify {
				req.Add("Security-Verify", v)
			}
			req.Add("Authorization", ua.authHeader(chal, uri, res, nil))
			resp, err = ua.tr.Request(ctx, req, nil)
		} else {
			break
		}
		if err != nil {
			return err
		}
	}
	if resp.StatusCode != 200 {
		detail := ""
		for _, h := range []string{"Warning", "Reason", "Server", "Retry-After"} {
			if v := resp.Get(h); v != "" {
				detail += " " + h + ": " + v + ";"
			}
		}
		return fmt.Errorf("ims: REGISTER rejected: %d %s%s", resp.StatusCode, resp.Reason, strings.TrimSuffix(detail, ";"))
	}
	if strings.Contains(resp.Get("Via"), "/TCP ") {
		ua.tr.PreferTCP = true // later requests follow the registration transport
	}
	ua.registered200(resp)
	return nil
}

// addSecurity adds sec-agree headers to non-REGISTER requests
// (RFC 3329 §2.3.1); several P-CSCFs answer 494 without them.
func (ua *UA) addSecurity(m *Message) {
	ua.mu.Lock()
	verify := append([]string(nil), ua.secVerify...)
	ua.mu.Unlock()
	if len(verify) == 0 {
		return
	}
	m.Add("Require", "sec-agree")
	m.Add("Proxy-Require", "sec-agree")
	for _, v := range verify {
		m.Add("Security-Verify", v)
	}
}

func (ua *UA) answerChallenge(ctx context.Context, resp *Message, offer *secOffer, uri string) (*Message, error) {
	var chal map[string]string
	for _, v := range resp.Values("WWW-Authenticate") {
		p := authParams(v)
		if strings.EqualFold(p["scheme"], "Digest") && strings.HasPrefix(strings.ToUpper(p["algorithm"]), "AKAV1") {
			chal = p
			break
		}
	}
	if chal == nil {
		return nil, errors.New("ims: 401 without AKA digest challenge")
	}
	nonce, err := base64.StdEncoding.DecodeString(chal["nonce"])
	if err != nil || len(nonce) < 32 {
		return nil, errors.New("ims: malformed AKA nonce")
	}
	res, ck, ik, auts, err := ua.cfg.AKA(ctx, nonce[:16], nonce[16:32])
	if err != nil {
		return nil, fmt.Errorf("ims: SIM rejected the network challenge: %w", err)
	}
	if auts != nil {
		// Resynchronise: resend with AUTS and an empty password.
		ua.log.Warn("ims: AKA sync failure, resynchronising")
		req := ua.newRegister(ua.cfg.Expires, offer)
		req.Add("Authorization", ua.authHeader(chal, uri, nil, auts))
		return ua.tr.Request(ctx, req, nil)
	}
	ua.mu.Lock()
	ua.nonceCount = 0
	ua.lastChal, ua.lastRES = chal, res
	ua.mu.Unlock()

	var verify []string
	if offer != nil {
		chosen, all, err := chooseSecurityServer(resp.Values("Security-Server"))
		if err != nil {
			return nil, err
		}
		verify = all
		if err := ua.installSAs(*offer, chosen, ck, ik); err != nil {
			return nil, err
		}
		ua.mu.Lock()
		ua.secVerify = append([]string(nil), all...)
		ua.mu.Unlock()
	}
	req := ua.newRegister(ua.cfg.Expires, offer)
	for _, v := range verify {
		req.Add("Security-Verify", v)
	}
	req.Add("Authorization", ua.authHeader(chal, uri, res, nil))
	return ua.tr.Request(ctx, req, nil)
}

type secServer struct {
	alg, ealg    string
	spiC, spiS   uint32
	portC, portS int
	q            float64
}

func chooseSecurityServer(vals []string) (secServer, []string, error) {
	var best *secServer
	for _, v := range vals {
		if !strings.HasPrefix(strings.TrimSpace(v), "ipsec-3gpp") {
			continue
		}
		get := func(k string) string { s, _ := Param(v, k); return s }
		s := secServer{alg: get("alg"), ealg: get("ealg"), q: 0.1}
		if s.ealg == "" {
			s.ealg = "null"
		}
		if q, err := strconv.ParseFloat(get("q"), 64); err == nil {
			s.q = q
		}
		c, _ := strconv.ParseUint(get("spi-c"), 10, 32)
		sv, _ := strconv.ParseUint(get("spi-s"), 10, 32)
		s.spiC, s.spiS = uint32(c), uint32(sv)
		s.portC, _ = strconv.Atoi(get("port-c"))
		s.portS, _ = strconv.Atoi(get("port-s"))
		if s.portC == 0 || s.portS == 0 || s.spiC == 0 || s.spiS == 0 {
			continue
		}
		if _, _, err := saAlgorithms(s.alg, s.ealg); err != nil {
			continue
		}
		if best == nil || s.q > best.q {
			cp := s
			best = &cp
		}
	}
	if best == nil {
		return secServer{}, nil, errors.New("ims: no acceptable Security-Server mechanism")
	}
	return *best, vals, nil
}

// saAlgorithms maps sec-agree names to ESP algorithms.
func saAlgorithms(alg, ealg string) (string, string, error) {
	var integ, enc string
	switch alg {
	case "hmac-sha-1-96":
		integ = "sha1"
	case "hmac-md5-96":
		integ = "md5"
	default:
		return "", "", fmt.Errorf("ims: unsupported alg %s", alg)
	}
	switch ealg {
	case "null", "":
		enc = "null"
	case "aes-cbc":
		enc = "aes128"
	case "des-ede3-cbc":
		enc = "3des"
	default:
		return "", "", fmt.Errorf("ims: unsupported ealg %s", ealg)
	}
	return enc, integ, nil
}

// installSAs derives the IMS IPsec keys (TS 33.203 Annex H/I) and installs
// the four transport-mode SAs.
func (ua *UA) installSAs(offer secOffer, srv secServer, ck, ik []byte) error {
	enc, integ, _ := saAlgorithms(srv.alg, srv.ealg)
	suite, err := ike.SuiteByName(enc, integ)
	if err != nil {
		return err
	}
	authKey := append([]byte(nil), ik...)
	if integ == "sha1" {
		authKey = append(authKey, 0, 0, 0, 0)
	}
	var encKey []byte
	switch enc {
	case "aes128":
		encKey = append([]byte(nil), ck...)
	case "3des":
		encKey = append(append(append([]byte(nil), ck[:8]...), ck[8:16]...), ck[:8]...)
	}
	mk := func(spi uint32) (*ike.ESPSA, error) { return ike.NewESPSA(spi, suite, encKey, authKey) }
	outCS, err := mk(srv.spiS) // UE port-c -> P-CSCF port-s
	if err != nil {
		return err
	}
	outSC, _ := mk(srv.spiC) // UE port-s -> P-CSCF port-c
	inC, _ := mk(offer.spiC) // P-CSCF port-s -> UE port-c
	inS, _ := mk(offer.spiS) // P-CSCF port-c -> UE port-s
	pc := ua.tr.PCSCF
	ua.cfg.Stack.SetTransportSAs([]netio.TransportRule{
		{Remote: pc, SrcPort: uint16(ua.tr.PortC), DstPort: uint16(srv.portS), SA: outCS},
		{Remote: pc, SrcPort: uint16(ua.tr.PortS), DstPort: uint16(srv.portC), SA: outSC},
	}, []*ike.ESPSA{inC, inS})
	ua.tr.ResetTCP()
	ua.tr.RemoteC, ua.tr.RemoteS = srv.portC, srv.portS
	ua.tr.Protected = true
	ua.mu.Lock()
	ua.secMech = srv.alg + "/" + srv.ealg
	ua.mu.Unlock()
	ua.log.Info("ims: sec-agree SAs installed", "alg", srv.alg, "ealg", srv.ealg, "pcscf_ports", fmt.Sprintf("%d/%d", srv.portC, srv.portS))
	return nil
}

func (ua *UA) registered200(resp *Message) {
	ua.mu.Lock()
	ua.serviceRoute = resp.Values("Service-Route")
	ua.assocURIs = nil
	for _, u := range resp.Values("P-Associated-URI") {
		ua.assocURIs = append(ua.assocURIs, URIOf(u))
	}
	exp := ua.cfg.Expires
	if v := resp.Get("Expires"); v != "" {
		exp, _ = strconv.Atoi(v)
	}
	for _, c := range resp.Values("Contact") {
		if strings.Contains(c, ua.tr.HostPort(ua.tr.PortS)) || strings.Contains(c, strconv.Itoa(ua.tr.PortS)) {
			if v, ok := Param(c, "expires"); ok {
				exp, _ = strconv.Atoi(v)
			}
		}
	}
	ua.expires = exp
	ua.registered = true
	ua.mu.Unlock()
	reg := ua.Registration()
	ua.log.Info("ims: registered", "identity_count", len(reg.URIs), "expires", exp, "security", reg.Security)
	ua.emit("registration", reg)
}

func (ua *UA) refreshLoop() {
	for {
		ua.mu.Lock()
		exp := ua.expires
		ua.mu.Unlock()
		wait := time.Duration(exp) * time.Second / 2
		if exp > 1200 {
			wait = time.Duration(exp-600) * time.Second
		}
		if wait < 30*time.Second {
			wait = 30 * time.Second
		}
		select {
		case <-ua.stop:
			return
		case err := <-ua.tr.failures:
			ua.registrationFailed(err)
			return
		case <-time.After(wait):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := ua.register(ctx, false)
		cancel()
		if err != nil {
			ua.registrationFailed(err)
			return
		}
	}
}

// Done is closed when the UA stopped (deregistered or failed).
func (ua *UA) Done() <-chan struct{} { return ua.done }

func (ua *UA) registrationFailed(err error) {
	ua.log.Warn("ims: registration lost", "err", err)
	ua.mu.Lock()
	ua.registered = false
	ua.mu.Unlock()
	ua.emit("registration", Registration{Registered: false, Error: err.Error()})
	ua.doneOnce.Do(func() { close(ua.done) })
}

// Close deregisters (best effort) and releases sockets.
func (ua *UA) Close(ctx context.Context) {
	ua.stopped.Do(func() {
		close(ua.stop)
		ua.doneOnce.Do(func() { close(ua.done) })
		ua.calls.hangupAll()
		req := ua.newRegister(0, nil)
		req.Set("Contact", ua.contactHeader(0))
		ua.mu.Lock()
		chal, res := ua.lastChal, ua.lastRES
		ua.mu.Unlock()
		if chal != nil {
			req.Add("Authorization", ua.authHeader(chal, "sip:"+ua.cfg.Domain, res, nil))
		}
		ua.addRoute(req, false)
		_, _ = ua.tr.Request(ctx, req, nil)
		ua.tr.Close()
		ua.cfg.Stack.SetTransportSAs(nil, nil)
	})
}

// addRoute adds the Service-Route set for originating requests.
func (ua *UA) addRoute(m *Message, originating bool) {
	ua.mu.Lock()
	routes := append([]string(nil), ua.serviceRoute...)
	ua.mu.Unlock()
	if !originating {
		return
	}
	for _, r := range routes {
		m.Add("Route", r)
	}
}

// ------------------------------------------------------- incoming requests

func (ua *UA) handleRequest(m *Message) {
	ua.log.Debug("sip: recv", "method", m.Method, "bytes", len(m.Body))
	switch m.Method {
	case "MESSAGE":
		ua.sms.handleMessage(m)
	case "INVITE", "ACK", "BYE", "CANCEL", "PRACK", "UPDATE", "INFO":
		ua.calls.handleRequest(m)
	case "OPTIONS":
		r := NewResponse(m, 200, "OK")
		r.Add("Allow", "INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, MESSAGE, NOTIFY, OPTIONS")
		r.Add("Accept", "application/sdp, application/vnd.3gpp.sms")
		_ = ua.tr.Reply(m, r)
	case "NOTIFY":
		_ = ua.tr.Reply(m, NewResponse(m, 200, "OK"))
		if strings.Contains(m.Get("Event"), "reg") && strings.Contains(string(m.Body), `state="terminated"`) {
			ua.log.Warn("ims: network terminated the registration")
		}
	default:
		_ = ua.tr.Reply(m, NewResponse(m, 501, "Not Implemented"))
	}
}
