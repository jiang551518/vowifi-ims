package ike

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Config controls one SWu tunnel.
type Config struct {
	Identity       string // permanent NAI (IDi)
	APN            string // IDr FQDN, default "ims"
	AKA            AKAFunc
	Proposals      ProposalConfig
	IMEI           string // optional, for DEVICE_IDENTITY
	DeviceIdentity bool
	NoIPv4         bool
	NoIPv6         bool
	Logger         *slog.Logger
	Rand           io.Reader
	Retransmit     time.Duration // initial retransmission timeout (default 1s)
	Attempts       int           // transmissions per request (default 6)
	DPD            time.Duration // liveness check interval (default 60s)
	Keepalive      time.Duration // NAT keepalive interval (default 20s)
	// Progress, if set, is called as the setup advances: "ike" (IKE SA
	// negotiated), "auth" (EAP-AKA succeeded).
	Progress func(step string)
}

// TunnelInfo describes an established tunnel.
type TunnelInfo struct {
	EPDG          net.IP
	Local         net.IP
	IPv4          net.IP
	IPv6          net.IP
	IPv6Prefix    int
	PCSCF         []net.IP
	DNS           []net.IP
	IKESuite      string
	ESPSuite      string
	ResponderAuth string
}

type ikeSA struct {
	spiI, spiR uint64
	weInit     bool
	suite      Suite
	skD        []byte
	skPi, skPr []byte
	out, in    *cipherState
	nextReq    uint32 // our next request message ID
	peerNext   uint32 // next expected peer request ID
	lastResp   []byte
	lastRespID uint32
	hasResp    bool
	frags      map[uint32]*fragBuf
}

type fragBuf struct {
	total uint16
	first uint8
	parts map[uint16][]byte
}

// Session is an established (or establishing) SWu tunnel.
type Session struct {
	cfg Config
	tr  Transport
	log *slog.Logger
	rnd io.Reader

	mu       sync.Mutex
	sa       *ikeSA
	oldSA    *ikeSA
	child    *ChildSA
	oldChild *ChildSA
	pending  map[uint32]chan *Message
	reqMu    sync.Mutex
	natPort  int

	onPacket atomic.Value // func([]byte)
	lastRecv atomic.Int64
	info     TunnelInfo
	done     chan struct{}
	closeErr error
	closed   sync.Once
}

func (cfg *Config) defaults() {
	if cfg.APN == "" {
		cfg.APN = "ims"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.Retransmit == 0 {
		cfg.Retransmit = time.Second
	}
	if cfg.Attempts == 0 {
		cfg.Attempts = 6
	}
	if cfg.DPD == 0 {
		cfg.DPD = 60 * time.Second
	}
	if cfg.Keepalive == 0 {
		cfg.Keepalive = 20 * time.Second
	}
	cfg.Proposals = cfg.Proposals.withDefaults()
}

// Connect establishes the IKE SA and the ESP child SA over tr. On success the
// session owns tr.
func Connect(ctx context.Context, tr Transport, cfg Config) (*Session, error) {
	cfg.defaults()
	if cfg.AKA == nil || cfg.Identity == "" {
		return nil, errors.New("ike: identity and AKA function are required")
	}
	s := &Session{cfg: cfg, tr: tr, log: cfg.Logger, rnd: cfg.Rand, pending: map[uint32]chan *Message{},
		natPort: 500, done: make(chan struct{})}
	s.info.EPDG = tr.Remote()
	s.info.Local = tr.Local()
	go s.recvLoop()
	if err := s.establish(ctx); err != nil {
		s.fail(err)
		return nil, err
	}
	go s.maintain()
	return s, nil
}

// Info returns tunnel parameters.
func (s *Session) Info() TunnelInfo { return s.info }

// Done is closed when the tunnel is gone; Err reports why.
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.closeErr
	default:
		return nil
	}
}

// SetPacketHandler installs the receiver for decrypted inner IP packets.
func (s *Session) SetPacketHandler(f func([]byte)) { s.onPacket.Store(f) }

// SendPacket encrypts one inner IP packet into the tunnel.
func (s *Session) SendPacket(pkt []byte) error {
	if len(pkt) == 0 {
		return nil
	}
	s.mu.Lock()
	child := s.child
	s.mu.Unlock()
	if child == nil {
		return errors.New("ike: tunnel not established")
	}
	nh := NextIPv4
	if pkt[0]>>4 == 6 {
		nh = NextIPv6
	}
	esp, err := child.Out.Seal(pkt, nh)
	if err != nil {
		return err
	}
	return s.tr.Send(4500, esp)
}

// Close deletes the IKE SA (best effort) and releases the transport.
func (s *Session) Close() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	del := []byte{ProtoIKE, 0, 0, 0}
	_, _ = s.requestOnce(ctx, ExchangeInformational, []Payload{{Type: PayloadDelete, Body: del}}, 2)
	s.fail(errors.New("ike: closed"))
	return nil
}

func (s *Session) fail(err error) {
	s.closed.Do(func() {
		s.closeErr = err
		close(s.done)
		_ = s.tr.Close()
		s.mu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.mu.Unlock()
	})
}

// ------------------------------------------------------------- establish

func (s *Session) random(n int) []byte {
	b := make([]byte, n)
	_, _ = io.ReadFull(s.rnd, b)
	return b
}

func (s *Session) establish(ctx context.Context) error {
	// IKE_SA_INIT ------------------------------------------------------
	group := s.cfg.Proposals.IKEDH[0]
	var cookie []byte
	var dh dhGroup
	var ni, nr, msg1, msg2 []byte
	var resp *Message
	spiI := binary.BigEndian.Uint64(s.random(8)) | 1
	// RFC 7296 §2.6: a retry carrying the responder's COOKIE must repeat the
	// request otherwise unchanged; responders derive the cookie from Ni (and
	// SPIi / address), so a fresh nonce makes the retry fail. Only an
	// INVALID_KE_PAYLOAD retry needs a new KE.
	ni = s.random(32)
	natSrc := s.random(20)
	keGroup := uint16(0)
	for attempt := 0; ; attempt++ {
		if attempt > 4 {
			return errors.New("ike: IKE_SA_INIT did not converge")
		}
		if dh == nil || keGroup != group {
			var err error
			if dh, err = newDH(group, s.rnd); err != nil {
				return err
			}
			keGroup = group
		}
		var err error
		ke := append([]byte{byte(group >> 8), byte(group), 0, 0}, dh.Public()...)
		var ps []Payload
		if cookie != nil {
			ps = append(ps, Notify{Type: NotifyCookie, Data: cookie}.payload())
		}
		ps = append(ps,
			Payload{Type: PayloadSA, Body: encodeSA(buildProposals(ProtoIKE, nil, s.cfg.Proposals, group))},
			Payload{Type: PayloadKE, Body: ke},
			Payload{Type: PayloadNonce, Body: ni},
			// A random source hash makes the ePDG believe we are behind NAT so
			// that ESP is always UDP-encapsulated (needed for proxies).
			Notify{Type: NotifyNATDetectionSourceIP, Data: natSrc}.payload(),
			Notify{Type: NotifyNATDetectionDestIP, Data: natHash(spiI, 0, s.tr.Remote(), 500)}.payload(),
			Notify{Type: NotifyFragmentationSupported}.payload(),
			Notify{Type: NotifySignatureHashAlgorithms, Data: []byte{0, 2, 0, 3, 0, 4}}.payload(),
		)
		s.mu.Lock()
		s.sa = &ikeSA{spiI: spiI, weInit: true}
		s.mu.Unlock()
		msg1 = s.encodePlain(spiI, 0, ExchangeSAInit, FlagInitiator, 0, ps)
		resp, err = s.roundTrip(ctx, 0, msg1, 0)
		if err != nil {
			return fmt.Errorf("ike: IKE_SA_INIT: %w", err)
		}
		if n, ok := resp.notify(NotifyCookie); ok {
			cookie = append([]byte(nil), n.Data...)
			s.log.Info("ike: ePDG requested cookie")
			continue
		}
		if n, ok := resp.notify(NotifyInvalidKEPayload); ok && len(n.Data) >= 2 {
			group = binary.BigEndian.Uint16(n.Data)
			s.log.Info("ike: ePDG requested another DH group", "group", group)
			continue
		}
		if n, ok := resp.errorNotify(); ok {
			return &NotifyError{Type: n.Type, Data: n.Data}
		}
		msg2 = resp.raw
		break
	}
	saP, keP, nP := resp.find(PayloadSA), resp.find(PayloadKE), resp.find(PayloadNonce)
	if saP == nil || keP == nil || nP == nil {
		return errors.New("ike: IKE_SA_INIT response missing SA/KE/Nonce")
	}
	props, err := decodeSA(saP.Body)
	if err != nil || len(props) != 1 {
		return fmt.Errorf("ike: bad SA in IKE_SA_INIT response: %v", err)
	}
	suite, err := suiteFromProposal(props[0])
	if err != nil {
		return err
	}
	if suite.PRF == nil || suite.DH != group {
		return errors.New("ike: responder chose inconsistent PRF/DH")
	}
	if len(keP.Body) < 4 {
		return errors.New("ike: short KE")
	}
	shared, err := dh.Shared(keP.Body[4:])
	if err != nil {
		return err
	}
	nr = append([]byte(nil), nP.Body...)
	spiR := resp.Header.SPIr
	sa := s.deriveIKESA(suite, suite.PRF.prf(append(append([]byte(nil), ni...), nr...), shared), ni, nr, spiI, spiR, true)
	sa.nextReq = 1
	s.mu.Lock()
	s.sa = sa
	s.natPort = 4500
	s.mu.Unlock()
	s.info.IKESuite = suite.String()
	s.log.Info("ike: IKE_SA_INIT complete", "suite", suite.String())
	s.progress("ike")

	// IKE_AUTH ---------------------------------------------------------
	idi := append([]byte{3, 0, 0, 0}, s.cfg.Identity...)
	idr := append([]byte{2, 0, 0, 0}, s.cfg.APN...)
	inSPI := binary.BigEndian.Uint32(s.random(4)) | 0x01000000
	spiBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(spiBytes, inSPI)
	ps := []Payload{
		{Type: PayloadIDi, Body: idi},
		Notify{Type: NotifyInitialContact}.payload(),
		{Type: PayloadIDr, Body: idr},
		{Type: PayloadCP, Body: s.cpRequest()},
		{Type: PayloadSA, Body: encodeSA(buildProposals(ProtoESP, spiBytes, s.cfg.Proposals, 0))},
		{Type: PayloadTSi, Body: encodeTS(allTrafficSelectors())},
		{Type: PayloadTSr, Body: encodeTS(allTrafficSelectors())},
	}
	if s.cfg.DeviceIdentity && len(s.cfg.IMEI) >= 14 {
		ps = append(ps, Notify{Type: NotifyDeviceIdentity, Data: deviceIdentity(s.cfg.IMEI)}.payload())
	}
	resp, err = s.request(ctx, ExchangeAuth, ps)
	if err != nil {
		return fmt.Errorf("ike: IKE_AUTH: %w", err)
	}
	if n, ok := resp.errorNotify(); ok {
		return &NotifyError{Type: n.Type, Data: n.Data}
	}
	var idrResp []byte
	if p := resp.find(PayloadIDr); p != nil {
		idrResp = append([]byte(nil), p.Body...)
	}
	s.info.ResponderAuth = s.verifyResponderSignature(resp, msg2, ni, idrResp)

	eap := newEAPAKA(s.cfg.Identity, s.cfg.AKA)
	for round := 0; ; round++ {
		if round > 8 {
			return errors.New("ike: too many EAP rounds")
		}
		ep := resp.find(PayloadEAP)
		if ep == nil {
			if n, ok := resp.errorNotify(); ok {
				return &NotifyError{Type: n.Type, Data: n.Data}
			}
			return errors.New("ike: IKE_AUTH response without EAP")
		}
		out, success, eerr := eap.handle(ctx, ep.Body)
		if eap.last != "" && !success {
			s.log.Info("ike: EAP step", "round", round, "step", eap.last)
		}
		if success {
			break
		}
		if out == nil {
			return eerr
		}
		resp, err = s.request(ctx, ExchangeAuth, []Payload{{Type: PayloadEAP, Body: out}})
		if eerr != nil {
			return eerr
		}
		if err != nil {
			return fmt.Errorf("ike: EAP exchange: %w", err)
		}
		if n, ok := resp.errorNotify(); ok {
			return &NotifyError{Type: n.Type, Data: n.Data}
		}
	}
	s.log.Info("ike: EAP-AKA succeeded")
	s.progress("auth")

	// AUTH with the MSK (RFC 7296 §2.16).
	keyPad := suite.PRF.prf(eap.msk, []byte("Key Pad for IKEv2"))
	signed := append(append(append([]byte(nil), msg1...), nr...), suite.PRF.prf(sa.skPi, idi)...)
	auth := append([]byte{2, 0, 0, 0}, suite.PRF.prf(keyPad, signed)...)
	resp, err = s.request(ctx, ExchangeAuth, []Payload{{Type: PayloadAUTH, Body: auth}})
	if err != nil {
		return fmt.Errorf("ike: final IKE_AUTH: %w", err)
	}
	if n, ok := resp.errorNotify(); ok {
		return &NotifyError{Type: n.Type, Data: n.Data}
	}
	ap := resp.find(PayloadAUTH)
	if ap == nil || len(ap.Body) < 4 {
		return errors.New("ike: ePDG did not send AUTH")
	}
	rSigned := append(append(append([]byte(nil), msg2...), ni...), suite.PRF.prf(sa.skPr, idrResp)...)
	if !hmac.Equal(ap.Body[4:], suite.PRF.prf(keyPad, rSigned)) {
		return errors.New("ike: ePDG AUTH verification failed")
	}
	if s.info.ResponderAuth == "" {
		s.info.ResponderAuth = "msk"
	} else {
		s.info.ResponderAuth += "+msk"
	}
	if cp := resp.find(PayloadCP); cp != nil {
		s.parseCP(cp.Body)
	}
	if s.info.IPv4 == nil && s.info.IPv6 == nil {
		return errors.New("ike: ePDG assigned no inner address")
	}
	cs := resp.find(PayloadSA)
	if cs == nil {
		return errors.New("ike: no child SA in IKE_AUTH response")
	}
	cprops, err := decodeSA(cs.Body)
	if err != nil || len(cprops) != 1 || len(cprops[0].SPI) != 4 {
		return errors.New("ike: bad child SA proposal")
	}
	csuite, err := suiteFromProposal(cprops[0])
	if err != nil {
		return err
	}
	outSPI := binary.BigEndian.Uint32(cprops[0].SPI)
	child, err := childKeys(suite.PRF, sa.skD, append(append([]byte(nil), ni...), nr...), csuite, inSPI, outSPI, true)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.child = child
	s.mu.Unlock()
	s.info.ESPSuite = csuite.String()
	s.log.Info("ike: tunnel established", "ipv4", s.info.IPv4, "ipv6", s.info.IPv6, "pcscf", s.info.PCSCF, "esp", csuite.String())
	return nil
}

func (s *Session) progress(step string) {
	if s.cfg.Progress != nil {
		s.cfg.Progress(step)
	}
}

func natHash(spiI, spiR uint64, ip net.IP, port int) []byte {
	h := sha1.New()
	var b [16]byte
	binary.BigEndian.PutUint64(b[:], spiI)
	binary.BigEndian.PutUint64(b[8:], spiR)
	h.Write(b[:])
	if v4 := ip.To4(); v4 != nil {
		h.Write(v4)
	} else {
		h.Write(ip.To16())
	}
	h.Write([]byte{byte(port >> 8), byte(port)})
	return h.Sum(nil)
}

func deviceIdentity(imei string) []byte {
	digits := []byte(imei)
	if len(digits) > 15 {
		digits = digits[:15]
	}
	var bcd []byte
	for i := 0; i < len(digits); i += 2 {
		lo := digits[i] - '0'
		hi := byte(0xF)
		if i+1 < len(digits) {
			hi = digits[i+1] - '0'
		}
		bcd = append(bcd, hi<<4|lo)
	}
	l := 1 + len(bcd)
	return append([]byte{byte(l >> 8), byte(l), 1}, bcd...)
}

func (s *Session) cpRequest() []byte {
	b := []byte{1, 0, 0, 0} // CFG_REQUEST
	add := func(t uint16) { b = append(b, byte(t>>8), byte(t), 0, 0) }
	if !s.cfg.NoIPv4 {
		add(1)  // INTERNAL_IP4_ADDRESS
		add(3)  // INTERNAL_IP4_DNS
		add(20) // P_CSCF_IP4_ADDRESS
	}
	if !s.cfg.NoIPv6 {
		add(8)  // INTERNAL_IP6_ADDRESS
		add(10) // INTERNAL_IP6_DNS
		add(21) // P_CSCF_IP6_ADDRESS
	}
	return b
}

func (s *Session) parseCP(b []byte) {
	if len(b) < 4 {
		return
	}
	b = b[4:]
	for len(b) >= 4 {
		t := binary.BigEndian.Uint16(b) & 0x7fff
		l := int(binary.BigEndian.Uint16(b[2:]))
		if 4+l > len(b) {
			return
		}
		v := b[4 : 4+l]
		switch {
		case t == 1 && l == 4:
			s.info.IPv4 = net.IP(append([]byte(nil), v...))
		case t == 8 && l == 17:
			s.info.IPv6 = net.IP(append([]byte(nil), v[:16]...))
			s.info.IPv6Prefix = int(v[16])
		case (t == 3 || t == 10) && (l == 4 || l == 16):
			s.info.DNS = append(s.info.DNS, net.IP(append([]byte(nil), v...)))
		case (t == 20 || t == 21) && (l == 4 || l == 16 || l == 17):
			n := l
			if l == 17 {
				n = 16
			}
			s.info.PCSCF = append(s.info.PCSCF, net.IP(append([]byte(nil), v[:n]...)))
		}
		b = b[4+l:]
	}
}

func (s *Session) deriveIKESA(suite Suite, skeyseed, ni, nr []byte, spiI, spiR uint64, weInit bool) *ikeSA {
	prf := suite.PRF
	ek := suite.Enc.keyMaterial()
	ak := 0
	if suite.Integ != nil {
		ak = suite.Integ.keyLen
	}
	seed := append(append([]byte(nil), ni...), nr...)
	var spis [16]byte
	binary.BigEndian.PutUint64(spis[:], spiI)
	binary.BigEndian.PutUint64(spis[8:], spiR)
	seed = append(seed, spis[:]...)
	km := prf.prfPlus(skeyseed, seed, 3*prf.size+2*ak+2*ek)
	take := func(n int) []byte { v := km[:n]; km = km[n:]; return v }
	sa := &ikeSA{spiI: spiI, spiR: spiR, weInit: weInit, suite: suite, frags: map[uint32]*fragBuf{}}
	sa.skD = take(prf.size)
	ai, ar := take(ak), take(ak)
	ei, er := take(ek), take(ek)
	sa.skPi, sa.skPr = take(prf.size), take(prf.size)
	ci, _ := newCipherState(suite.Enc, suite.Integ, ei, ai)
	cr, _ := newCipherState(suite.Enc, suite.Integ, er, ar)
	if weInit {
		sa.out, sa.in = ci, cr
	} else {
		sa.out, sa.in = cr, ci
	}
	return sa
}

// ----------------------------------------------------------- message codec

func (s *Session) encodePlain(spiI, spiR uint64, exch, flags uint8, id uint32, ps []Payload) []byte {
	first, body := encodePayloads(ps)
	out := make([]byte, headerLen+len(body))
	Header{SPIi: spiI, SPIr: spiR, NextPayload: first, Exchange: exch, Flags: flags, MessageID: id, Length: uint32(len(out))}.marshal(out)
	copy(out[headerLen:], body)
	return out
}

func (s *Session) encodeSK(sa *ikeSA, exch uint8, id uint32, response bool, ps []Payload) ([]byte, error) {
	first, inner := encodePayloads(ps)
	pt := padTrailer(inner, sa.suite.Enc.block, -1)
	sealedLen := sa.suite.Enc.ivLen + len(pt) + sa.out.icvLen()
	total := headerLen + 4 + sealedLen
	flags := uint8(0)
	if sa.weInit {
		flags |= FlagInitiator
	}
	if response {
		flags |= FlagResponse
	}
	aad := make([]byte, headerLen+4)
	Header{SPIi: sa.spiI, SPIr: sa.spiR, NextPayload: PayloadSK, Exchange: exch, Flags: flags, MessageID: id, Length: uint32(total)}.marshal(aad)
	aad[headerLen] = first
	binary.BigEndian.PutUint16(aad[headerLen+2:], uint16(4+sealedLen))
	sealed, err := sa.out.seal(s.rnd, aad, pt)
	if err != nil {
		return nil, err
	}
	return append(aad, sealed...), nil
}

func (m *Message) setRaw(b []byte) { m.raw = b }

// decode parses a received IKE message, decrypting SK and reassembling SKF.
// It returns (nil, nil) while fragments are outstanding.
func (s *Session) decode(sa *ikeSA, b []byte, h Header) (*Message, error) {
	ps, err := decodePayloads(h.NextPayload, b[headerLen:h.Length])
	if err != nil {
		return nil, err
	}
	m := &Message{Header: h}
	m.setRaw(append([]byte(nil), b[:h.Length]...))
	for i, p := range ps {
		switch p.Type {
		case PayloadSK:
			if sa == nil || sa.in == nil {
				return nil, errors.New("ike: encrypted payload before keys")
			}
			off := int(h.Length) - len(p.Body)
			pt, err := sa.in.open(b[:off], p.Body)
			if err != nil {
				return nil, err
			}
			inner, err := stripIKEPad(pt)
			if err != nil {
				return nil, err
			}
			ips, err := decodePayloads(p.innerFirst, inner)
			if err != nil {
				return nil, err
			}
			m.Payloads = append(append(m.Payloads, ps[:i]...), ips...)
			return m, nil
		case PayloadSKF:
			if sa == nil || sa.in == nil || len(p.Body) < 4 {
				return nil, errors.New("ike: bad fragment")
			}
			num, total := binary.BigEndian.Uint16(p.Body), binary.BigEndian.Uint16(p.Body[2:])
			off := int(h.Length) - len(p.Body) + 4
			pt, err := sa.in.open(b[:off], p.Body[4:])
			if err != nil {
				return nil, err
			}
			inner, err := stripIKEPad(pt)
			if err != nil {
				return nil, err
			}
			key := h.MessageID<<1 | uint32(h.Flags&FlagResponse>>5)
			fb := sa.frags[key]
			if fb == nil || fb.total != total {
				fb = &fragBuf{total: total, parts: map[uint16][]byte{}}
				sa.frags[key] = fb
			}
			if num == 1 {
				fb.first = p.innerFirst
			}
			fb.parts[num] = inner
			if len(fb.parts) < int(total) {
				return nil, nil
			}
			delete(sa.frags, key)
			var all []byte
			for n := uint16(1); n <= total; n++ {
				all = append(all, fb.parts[n]...)
			}
			ips, err := decodePayloads(fb.first, all)
			if err != nil {
				return nil, err
			}
			m.Payloads = ips
			return m, nil
		}
	}
	m.Payloads = ps
	return m, nil
}

func stripIKEPad(pt []byte) ([]byte, error) {
	if len(pt) < 1 {
		return nil, errors.New("ike: empty plaintext")
	}
	pad := int(pt[len(pt)-1])
	if pad+1 > len(pt) {
		return nil, errors.New("ike: bad padding")
	}
	return pt[:len(pt)-1-pad], nil
}

// ------------------------------------------------------------- transport IO

func (s *Session) sendIKE(b []byte) error {
	s.mu.Lock()
	port := s.natPort
	s.mu.Unlock()
	if port == 4500 {
		b = append([]byte{0, 0, 0, 0}, b...)
	}
	return s.tr.Send(port, b)
}

// roundTrip sends an already encoded request and waits for the response
// with the given message ID, retransmitting with exponential back-off.
func (s *Session) roundTrip(ctx context.Context, id uint32, msg []byte, attempts int) (*Message, error) {
	if attempts <= 0 {
		attempts = s.cfg.Attempts
	}
	ch := make(chan *Message, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	wait := s.cfg.Retransmit
	for i := 0; i < attempts; i++ {
		if err := s.sendIKE(msg); err != nil {
			return nil, err
		}
		t := time.NewTimer(wait)
		select {
		case m, ok := <-ch:
			t.Stop()
			if !ok {
				return nil, s.closeErrOr()
			}
			return m, nil
		case <-t.C:
			wait *= 2
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-s.done:
			t.Stop()
			return nil, s.closeErrOr()
		}
	}
	return nil, errors.New("ike: no response from ePDG (timeout)")
}

func (s *Session) closeErrOr() error {
	if s.closeErr != nil {
		return s.closeErr
	}
	return errors.New("ike: session closed")
}

func (s *Session) request(ctx context.Context, exch uint8, ps []Payload) (*Message, error) {
	return s.requestOnce(ctx, exch, ps, 0)
}

func (s *Session) requestOnce(ctx context.Context, exch uint8, ps []Payload, attempts int) (*Message, error) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	s.mu.Lock()
	sa := s.sa
	if sa == nil || sa.out == nil {
		s.mu.Unlock()
		return nil, errors.New("ike: no IKE SA")
	}
	id := sa.nextReq
	sa.nextReq++
	s.mu.Unlock()
	msg, err := s.encodeSK(sa, exch, id, false, ps)
	if err != nil {
		return nil, err
	}
	return s.roundTrip(ctx, id, msg, attempts)
}

func (s *Session) recvLoop() {
	for {
		port, b, err := s.tr.Recv()
		if err != nil {
			s.fail(fmt.Errorf("ike: transport: %w", err))
			return
		}
		s.lastRecv.Store(time.Now().UnixNano())
		if port == 4500 {
			if len(b) == 1 && b[0] == 0xff {
				continue
			}
			if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 0 {
				s.handleIKE(b[4:])
			} else if len(b) >= 8 {
				s.handleESP(b)
			}
			continue
		}
		s.handleIKE(b)
	}
}

func (s *Session) handleESP(b []byte) {
	spi := binary.BigEndian.Uint32(b)
	s.mu.Lock()
	var sa *ESPSA
	if s.child != nil && s.child.In.SPI == spi {
		sa = s.child.In
	} else if s.oldChild != nil && s.oldChild.In.SPI == spi {
		sa = s.oldChild.In
	}
	s.mu.Unlock()
	if sa == nil {
		return
	}
	pkt, nh, err := sa.Open(b)
	if err != nil {
		s.log.Debug("esp: drop", "err", err)
		return
	}
	if nh != NextIPv4 && nh != NextIPv6 {
		return // dummy/TFC padding (59) or unexpected
	}
	if f, ok := s.onPacket.Load().(func([]byte)); ok && f != nil {
		f(pkt)
	}
}

func (s *Session) handleIKE(b []byte) {
	h, err := parseHeader(b)
	if err != nil {
		return
	}
	s.mu.Lock()
	var sa *ikeSA
	for _, c := range []*ikeSA{s.sa, s.oldSA} {
		if c != nil && c.spiI == h.SPIi && (c.spiR == h.SPIr || c.spiR == 0) {
			sa = c
			break
		}
	}
	s.mu.Unlock()
	if sa == nil {
		return
	}
	if sa.in == nil {
		// IKE_SA_INIT response.
		if h.Flags&FlagResponse == 0 || h.Exchange != ExchangeSAInit {
			return
		}
		m, err := s.decode(nil, b, h)
		if err == nil {
			s.deliver(h.MessageID, m)
		}
		return
	}
	m, err := s.decode(sa, b, h)
	if err != nil {
		s.log.Debug("ike: undecodable message", "err", err)
		return
	}
	if m == nil {
		return // waiting for more fragments
	}
	if h.Flags&FlagResponse != 0 {
		if sa == s.currentSA() {
			s.deliver(h.MessageID, m)
		}
		return
	}
	s.handlePeerRequest(sa, m)
}

func (s *Session) currentSA() *ikeSA {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sa
}

func (s *Session) deliver(id uint32, m *Message) {
	s.mu.Lock()
	ch := s.pending[id]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- m:
		default:
		}
	}
}

// ------------------------------------------------------- peer-initiated

func (s *Session) respond(sa *ikeSA, m *Message, ps []Payload) {
	out, err := s.encodeSK(sa, m.Header.Exchange, m.Header.MessageID, true, ps)
	if err != nil {
		return
	}
	sa.lastResp, sa.lastRespID, sa.hasResp = out, m.Header.MessageID, true
	_ = s.sendIKE(out)
}

func (s *Session) handlePeerRequest(sa *ikeSA, m *Message) {
	id := m.Header.MessageID
	if sa.hasResp && id == sa.lastRespID {
		_ = s.sendIKE(sa.lastResp) // retransmitted request
		return
	}
	if id != sa.peerNext {
		return
	}
	sa.peerNext++
	switch m.Header.Exchange {
	case ExchangeInformational:
		s.handleInformational(sa, m)
	case ExchangeCreateChild:
		s.handleCreateChild(sa, m)
	default:
		s.respond(sa, m, []Payload{Notify{Type: NotifyInvalidSyntax}.payload()})
	}
}

func (s *Session) handleInformational(sa *ikeSA, m *Message) {
	var reply []Payload
	ending := false
	for _, p := range m.all(PayloadDelete) {
		if len(p.Body) < 4 {
			continue
		}
		switch p.Body[0] {
		case ProtoIKE:
			s.mu.Lock()
			if sa == s.oldSA {
				s.oldSA = nil
			} else {
				ending = true
			}
			s.mu.Unlock()
		case ProtoESP:
			n := int(binary.BigEndian.Uint16(p.Body[2:]))
			var ours []byte
			for i := 0; i < n && 4+i*4+4 <= len(p.Body); i++ {
				spi := binary.BigEndian.Uint32(p.Body[4+i*4:])
				s.mu.Lock()
				switch {
				case s.oldChild != nil && s.oldChild.Out.SPI == spi:
					ours = binary.BigEndian.AppendUint32(ours, s.oldChild.In.SPI)
					s.oldChild = nil
				case s.child != nil && s.child.Out.SPI == spi:
					ours = binary.BigEndian.AppendUint32(ours, s.child.In.SPI)
					ending = true
				}
				s.mu.Unlock()
			}
			if len(ours) > 0 {
				body := []byte{ProtoESP, 4, byte(len(ours) / 4 >> 8), byte(len(ours) / 4)}
				reply = append(reply, Payload{Type: PayloadDelete, Body: append(body, ours...)})
			}
		}
	}
	s.respond(sa, m, reply)
	if ending {
		s.log.Warn("ike: ePDG deleted the tunnel")
		s.fail(errors.New("ike: tunnel deleted by ePDG"))
	}
}

// selectProposal picks the first acceptable transform set (responder role).
func (s *Session) selectProposal(props []Proposal, proto uint8, keGroup uint16) (Proposal, Suite, error) {
	cfg := s.cfg.Proposals
	allowedEnc := map[string]bool{}
	for _, n := range append(append([]string{}, cfg.IKEEnc...), cfg.ESPEnc...) {
		allowedEnc[n] = true
	}
	for _, p := range props {
		if p.Protocol != proto {
			continue
		}
		var pick []Transform
		have := map[uint8]bool{}
		for _, t := range p.Transforms {
			if have[t.Type] {
				continue
			}
			ok := false
			switch t.Type {
			case TransformENCR:
				for _, e := range encAlgs {
					if e.id == t.ID && (!e.keyAttr || uint16(e.keyLen*8) == t.KeyLen) && allowedEnc[e.name] {
						ok = true
					}
				}
			case TransformINTEG:
				for _, a := range integAlgs {
					ok = ok || a.id == t.ID
				}
			case TransformPRF:
				for _, a := range prfAlgs {
					ok = ok || a.id == t.ID
				}
			case TransformDH:
				ok = t.ID == keGroup || keGroup == 0
			case TransformESN:
				ok = t.ID == 0
			}
			if ok {
				pick = append(pick, t)
				have[t.Type] = true
			}
		}
		chosen := Proposal{Num: p.Num, Protocol: p.Protocol, Transforms: pick}
		suite, err := suiteFromProposal(chosen)
		if err != nil {
			continue
		}
		if proto == ProtoIKE && (suite.PRF == nil || suite.DH == 0) {
			continue
		}
		return chosen, suite, nil
	}
	return Proposal{}, Suite{}, errors.New("no acceptable proposal")
}

func (s *Session) handleCreateChild(sa *ikeSA, m *Message) {
	saP, nP := m.find(PayloadSA), m.find(PayloadNonce)
	if saP == nil || nP == nil {
		s.respond(sa, m, []Payload{Notify{Type: NotifyInvalidSyntax}.payload()})
		return
	}
	props, err := decodeSA(saP.Body)
	if err != nil || len(props) == 0 {
		s.respond(sa, m, []Payload{Notify{Type: NotifyNoProposalChosen}.payload()})
		return
	}
	var keGroup uint16
	var dh dhGroup
	var shared []byte
	keP := m.find(PayloadKE)
	if keP != nil && len(keP.Body) >= 4 {
		keGroup = binary.BigEndian.Uint16(keP.Body)
	}
	proto := props[0].Protocol
	chosen, suite, err := s.selectProposal(props, proto, keGroup)
	if err != nil {
		s.respond(sa, m, []Payload{Notify{Type: NotifyNoProposalChosen}.payload()})
		return
	}
	if keGroup != 0 {
		if dh, err = newDH(keGroup, s.rnd); err == nil {
			shared, err = dh.Shared(keP.Body[4:])
		}
		if err != nil {
			s.respond(sa, m, []Payload{Notify{Type: NotifyInvalidKEPayload, Data: []byte{0, 14}}.payload()})
			return
		}
	}
	peerNonce := nP.Body
	ourNonce := s.random(32)
	var reply []Payload

	if proto == ProtoIKE {
		// IKE SA rekey: the ePDG becomes the initiator of the new SA.
		if len(props[0].SPI) != 8 || dh == nil {
			s.respond(sa, m, []Payload{Notify{Type: NotifyInvalidSyntax}.payload()})
			return
		}
		newSPIi := binary.BigEndian.Uint64(props[0].SPI)
		newSPIr := binary.BigEndian.Uint64(s.random(8)) | 1
		chosen.SPI = binary.BigEndian.AppendUint64(nil, newSPIr)
		reply = []Payload{
			{Type: PayloadSA, Body: encodeSA([]Proposal{chosen})},
			{Type: PayloadNonce, Body: ourNonce},
			{Type: PayloadKE, Body: append([]byte{byte(keGroup >> 8), byte(keGroup), 0, 0}, dh.Public()...)},
		}
		s.respond(sa, m, reply)
		seed := append(append(append([]byte(nil), shared...), peerNonce...), ourNonce...)
		skeyseed := sa.suite.PRF.prf(sa.skD, seed)
		nsa := s.deriveIKESA(suite, skeyseed, peerNonce, ourNonce, newSPIi, newSPIr, false)
		s.mu.Lock()
		s.oldSA, s.sa = s.sa, nsa
		s.mu.Unlock()
		s.info.IKESuite = suite.String()
		s.log.Info("ike: IKE SA rekeyed by ePDG", "suite", suite.String())
		return
	}

	// Child SA rekey (or a new child SA, which we treat the same).
	if len(props[0].SPI) != 4 {
		s.respond(sa, m, []Payload{Notify{Type: NotifyInvalidSyntax}.payload()})
		return
	}
	outSPI := binary.BigEndian.Uint32(props[0].SPI)
	inSPI := binary.BigEndian.Uint32(s.random(4)) | 0x01000000
	chosen.SPI = binary.BigEndian.AppendUint32(nil, inSPI)
	reply = []Payload{
		{Type: PayloadSA, Body: encodeSA([]Proposal{chosen})},
		{Type: PayloadNonce, Body: ourNonce},
	}
	if dh != nil {
		reply = append(reply, Payload{Type: PayloadKE, Body: append([]byte{byte(keGroup >> 8), byte(keGroup), 0, 0}, dh.Public()...)})
	}
	if ts := m.find(PayloadTSi); ts != nil {
		reply = append(reply, Payload{Type: PayloadTSi, Body: ts.Body})
	}
	if ts := m.find(PayloadTSr); ts != nil {
		reply = append(reply, Payload{Type: PayloadTSr, Body: ts.Body})
	}
	seed := append(append(append([]byte(nil), shared...), peerNonce...), ourNonce...)
	child, err := childKeys(sa.suite.PRF, sa.skD, seed, suite, inSPI, outSPI, false)
	if err != nil {
		s.respond(sa, m, []Payload{Notify{Type: NotifyNoProposalChosen}.payload()})
		return
	}
	s.respond(sa, m, reply)
	s.mu.Lock()
	s.oldChild, s.child = s.child, child
	s.mu.Unlock()
	s.info.ESPSuite = suite.String()
	s.log.Info("ike: child SA rekeyed by ePDG", "suite", suite.String())
}

// ------------------------------------------------------------- liveness

func (s *Session) maintain() {
	ka := time.NewTicker(s.cfg.Keepalive)
	defer ka.Stop()
	s.lastRecv.Store(time.Now().UnixNano())
	for {
		select {
		case <-s.done:
			return
		case <-ka.C:
			_ = s.tr.Send(4500, []byte{0xff})
			idle := time.Since(time.Unix(0, s.lastRecv.Load()))
			if idle >= s.cfg.DPD {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, err := s.requestOnce(ctx, ExchangeInformational, nil, 5)
				cancel()
				if err != nil {
					s.log.Warn("ike: dead peer detected", "err", err)
					s.fail(fmt.Errorf("ike: ePDG unreachable: %w", err))
					return
				}
			}
		}
	}
}
