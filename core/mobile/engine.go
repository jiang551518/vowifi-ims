// Package mobile is the gomobile-facing API of the VoWiFi core. All
// structured data crosses the boundary as JSON strings.
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"vowifi/carrier"
	"vowifi/esim"
	"vowifi/ike"
	"vowifi/ims"
	"vowifi/netio"
	"vowifi/sim"
)

// Listener receives events from the engine (implemented in Kotlin).
type Listener interface {
	// OnEvent: kind is status | registration | sms | sms_status | call | log.
	OnEvent(kind string, json string)
	// OnAudio delivers one received AMR storage-format frame.
	OnAudio(callID string, frame []byte)
	// OnDTMF reports a received DTMF digit.
	OnDTMF(callID string, digit string)
}

// Resolver resolves a host on the selected Android network; returns a
// comma-separated list of IP literals.
type Resolver interface {
	Resolve(host string, networkHandle int64) (string, error)
}

// Config is the JSON accepted by Start.
type Config struct {
	SubID         int      `json:"subId"`
	Network       string   `json:"network"` // auto | bound
	NetworkHandle int64    `json:"networkHandle"`
	SOCKS5        string   `json:"socks5"`
	SOCKSUser     string   `json:"socksUser"`
	SOCKSPass     string   `json:"socksPass"`
	DNS           string   `json:"dns"` // system | doh | static
	DoH           []string `json:"doh"`
	StaticIP      string   `json:"staticIp"`
	PreferIPv6    bool     `json:"preferIpv6"`
	App           int      `json:"app"` // 2 USIM, 5 ISIM, 0 auto
	// Optional overrides of the carrier profile.
	EPDG        string             `json:"epdg"`
	DialScheme  string             `json:"dialScheme"`
	SecAgree    *bool              `json:"secAgree"`
	PreferTCP   *bool              `json:"preferTcp"`
	DeviceID    *bool              `json:"deviceIdentity"`
	CountryCode string             `json:"countryCode"`
	SMSC        string             `json:"smsc"`
	Proposals   ike.ProposalConfig `json:"proposals"`
	IMSDomain   string             `json:"imsDomain"`
	IMPIRealm   string             `json:"impiRealm"`
	NAIRealm    string             `json:"naiRealm"`
	PCSCF       []string           `json:"pcscf"`
	PANI        string             `json:"pani"`
	APN         string             `json:"apn"`
	InstanceID  string             `json:"instanceId"` // UUID for +sip.instance; empty = IMEI-based
}

// Status is reported with every phase change.
type Status struct {
	Phase string `json:"phase"` // idle connecting tunnel registering registered waiting error
	// Step is the active setup step: resolve ike auth tunnel ims ready.
	// FailedStep names the step where the last attempt failed.
	Step       string            `json:"step,omitempty"`
	FailedStep string            `json:"failedStep,omitempty"`
	PLMN       string            `json:"plmn,omitempty"`
	Message    string            `json:"message,omitempty"`
	Error      string            `json:"error,omitempty"`
	Attempt    int               `json:"attempt"`
	Carrier    string            `json:"carrier,omitempty"`
	EPDG       string            `json:"epdg,omitempty"`
	EPDGIP     string            `json:"epdgIp,omitempty"`
	InnerIPv4  string            `json:"innerIpv4,omitempty"`
	InnerIPv6  string            `json:"innerIpv6,omitempty"`
	PCSCF      []string          `json:"pcscf,omitempty"`
	IKE        string            `json:"ike,omitempty"`
	ESP        string            `json:"esp,omitempty"`
	Reg        *ims.Registration `json:"registration,omitempty"`
	Since      time.Time         `json:"since"`
	NextRetry  time.Time         `json:"nextRetry,omitempty"`
}

// Engine owns the root helper and the connection supervisor.
type Engine struct {
	classpath string
	listener  Listener
	resolver  Resolver

	mu        sync.Mutex
	helper    *sim.Helper
	cancel    context.CancelFunc
	kick      chan struct{}
	status    Status
	ua        *ims.UA
	rawSMS    bool // incoming SMS go to the platform SMS stack (system IMS service)
	running   bool
	lastPCSCF string
	// gen identifies the current supervisor; done is closed when it exits.
	// A replaced supervisor must not touch the state of its successor.
	gen  int
	done chan struct{}
}

// NewEngine creates an engine. classpath is the APK path (for RootHelper).
func NewEngine(classpath string, l Listener) *Engine {
	restoreEnv()
	e := &Engine{classpath: classpath, listener: l, status: Status{Phase: "idle", Since: time.Now()}}
	slog.SetDefault(slog.New(&logHandler{e: e, level: slog.LevelInfo}))
	return e
}

// SetResolver installs the Android network resolver.
func (e *Engine) SetResolver(r Resolver) { e.resolver = r }

// SetDebug toggles debug logging.
func (e *Engine) SetDebug(on bool) {
	lv := slog.LevelInfo
	if on {
		lv = slog.LevelDebug
	}
	slog.SetDefault(slog.New(&logHandler{e: e, level: lv}))
}

func (e *Engine) emit(kind string, v any) {
	b, _ := json.Marshal(v)
	if e.listener != nil {
		e.listener.OnEvent(kind, string(b))
	}
}

func (e *Engine) setStatus(f func(*Status)) {
	e.mu.Lock()
	f(&e.status)
	st := e.status
	e.mu.Unlock()
	e.emit("status", st)
}

func (e *Engine) phase(p, msg string) {
	e.setStatus(func(s *Status) {
		if s.Phase != p {
			s.Since = time.Now()
		}
		s.Phase, s.Message = p, msg
		if p != "error" && p != "waiting" {
			s.Error = ""
		}
	})
}

// StatusJSON returns the current status.
func (e *Engine) StatusJSON() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := json.Marshal(e.status)
	return string(b)
}

func (e *Engine) getHelper(ctx context.Context) (*sim.Helper, error) {
	e.mu.Lock()
	h := e.helper
	e.mu.Unlock()
	if h != nil {
		select {
		case <-h.Done():
		default:
			return h, nil
		}
	}
	h, err := sim.StartHelper(ctx, e.classpath, "su")
	if err != nil {
		return nil, fmt.Errorf("root access failed: %w", err)
	}
	h.Log = func(s string) { slog.Debug(s) }
	e.mu.Lock()
	e.helper = h
	e.mu.Unlock()
	return h, nil
}

// CheckRoot starts the root helper and reports the result.
func (e *Engine) CheckRoot() error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	_, err := e.getHelper(ctx)
	return err
}

// SubscriptionsJSON lists active SIMs together with their carrier profile.
func (e *Engine) SubscriptionsJSON() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return "", err
	}
	subs, err := h.Subscriptions(ctx)
	if err != nil {
		return "", err
	}
	type entry struct {
		sim.Subscription
		Profile carrier.Profile `json:"profile"`
		EPDG    string          `json:"epdg"`
	}
	var out []entry
	for _, s := range subs {
		p := carrier.Match(s.MCC, s.MNC, s.CarrierName+" "+s.DisplayName, s.ICCID)
		out = append(out, entry{Subscription: s, Profile: p, EPDG: p.EPDGHost(s.MCC, s.MNC)})
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// CarriersJSON lists built-in carrier profiles.
func (e *Engine) CarriersJSON() string {
	b, _ := json.Marshal(carrier.All())
	return string(b)
}

// Start begins (or restarts) the supervised connection.
func (e *Engine) Start(configJSON string) error {
	var cfg Config
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("bad config: %w", err)
	}
	// Claim the new generation before stopping the old loop: a replaced loop is
	// then no longer current and does not report "idle" (a restart is not a
	// stop, and the app keeps the system registration through a quick restart).
	e.mu.Lock()
	e.gen++
	gen := e.gen
	e.mu.Unlock()
	e.Stop()
	// Let the previous session finish tearing down first: otherwise its late
	// cleared UA overwrites the new, registered session.
	e.mu.Lock()
	old := e.done
	e.mu.Unlock()
	if old != nil {
		select {
		case <-old:
		case <-time.After(15 * time.Second):
			slog.Warn("vowifi: previous session still stopping, starting anyway")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.mu.Lock()
	e.cancel = cancel
	e.kick = make(chan struct{}, 1)
	e.running = true
	e.done = done
	kick := e.kick
	e.mu.Unlock()
	go func() {
		defer close(done)
		e.supervise(ctx, cfg, kick, gen)
	}()
	return nil
}

// Stop tears everything down.
func (e *Engine) Stop() {
	e.mu.Lock()
	cancel := e.cancel
	e.cancel = nil
	e.running = false
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// NetworkChanged forces an immediate reconnect (e.g. Wi-Fi <-> cellular).
func (e *Engine) NetworkChanged() {
	e.mu.Lock()
	k := e.kick
	e.mu.Unlock()
	if k != nil {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

func (e *Engine) supervise(ctx context.Context, cfg Config, kick chan struct{}, gen int) {
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	attempt := 0
	for ctx.Err() == nil {
		attempt++
		e.setStatus(func(s *Status) { s.Attempt = attempt; s.NextRetry = time.Time{} })
		err := e.session(ctx, cfg, kick, func() { attempt = 0 })
		if ctx.Err() != nil {
			break
		}
		// attempt is reset to 0 once a session registers, so a session that
		// ends later (e.g. network change) must still pick backoff[0].
		wait := backoff[min(max(attempt, 1), len(backoff))-1]
		var ne *ike.NotifyError
		if errors.As(err, &ne) || errors.Is(err, ike.ErrEAPFailure) {
			wait = 60 * time.Second // the network refused us; do not hammer it
		}
		if err != nil && strings.Contains(err.Error(), "REGISTER rejected: 403") {
			// The IMS core forbids this subscriber/location (e.g. Globe's
			// "Service not allowed in this location"); retrying soon will not
			// help. A network change (kick) or manual restart still retries now.
			wait = 10 * time.Minute
		}
		msg := "disconnected"
		if err != nil {
			msg = err.Error()
			slog.Warn("vowifi: session ended", "err", err)
		}
		e.setStatus(func(s *Status) {
			s.Phase, s.Error, s.Since, s.NextRetry = "waiting", msg, time.Now(), time.Now().Add(wait)
			if s.Step != "" && s.Step != "ready" {
				s.FailedStep = s.Step
			} else {
				s.FailedStep = "ims" // registration was lost after being ready
			}
			s.Reg = nil
		})
		select {
		case <-ctx.Done():
		case <-kick:
		case <-time.After(wait):
		}
	}
	e.mu.Lock()
	current := e.gen == gen
	if current {
		e.ua = nil
	}
	e.mu.Unlock()
	if current {
		e.setStatus(func(s *Status) { *s = Status{Phase: "idle", Since: time.Now()} })
	}
}

func (e *Engine) netConfig(cfg Config) ike.NetConfig {
	nc := ike.NetConfig{SOCKS5: cfg.SOCKS5, SOCKSUser: cfg.SOCKSUser, SOCKSPass: cfg.SOCKSPass,
		DNSMode: cfg.DNS, DoHServers: cfg.DoH, PreferIPv6: cfg.PreferIPv6}
	if cfg.StaticIP != "" {
		nc.StaticIPs = strings.Split(cfg.StaticIP, ",")
	}
	if cfg.Network == "bound" && cfg.NetworkHandle != 0 {
		handle := cfg.NetworkHandle
		nc.Control = func(network, address string, c syscall.RawConn) error {
			var berr error
			if err := c.Control(func(fd uintptr) { berr = bindToNetwork(handle, int(fd)) }); err != nil {
				return err
			}
			return berr
		}
	}
	if e.resolver != nil {
		r, handle := e.resolver, int64(0)
		if cfg.Network == "bound" {
			handle = cfg.NetworkHandle
		}
		nc.Lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			s, err := r.Resolve(host, handle)
			if err != nil {
				return nil, err
			}
			var ips []net.IP
			for _, p := range strings.Split(s, ",") {
				if ip := net.ParseIP(strings.TrimSpace(p)); ip != nil {
					ips = append(ips, ip)
				}
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("%s: no address", host)
			}
			return ips, nil
		}
	}
	return nc
}

// session runs one tunnel + IMS lifetime.
func (e *Engine) session(ctx context.Context, cfg Config, kick chan struct{}, onRegistered func()) error {
	e.setStatus(func(s *Status) { s.Step, s.FailedStep = "resolve", "" })
	e.phase("connecting", "reading SIM")
	h, err := e.getHelper(ctx)
	if err != nil {
		return err
	}
	subs, err := h.Subscriptions(ctx)
	if err != nil {
		return err
	}
	var sub *sim.Subscription
	for i := range subs {
		if subs[i].SubID == cfg.SubID {
			sub = &subs[i]
		}
	}
	if sub == nil {
		return fmt.Errorf("SIM (subId %d) is not active", cfg.SubID)
	}
	id, err := h.Identity(ctx, sub.SubID, sub.Slot)
	if err != nil {
		return err
	}
	prof := carrier.Match(sub.MCC, sub.MNC, sub.CarrierName+" "+sub.DisplayName, sub.ICCID)
	applyOverrides(&prof, cfg)
	host := prof.EPDGHost(sub.MCC, sub.MNC)
	smsc := id.SMSC
	if cfg.SMSC != "" {
		smsc = cfg.SMSC
	}
	e.setStatus(func(s *Status) { s.Carrier, s.EPDG, s.PLMN = prof.Name, host, sub.MCC+sub.MNC })

	app := cfg.App
	if app == 0 {
		app = sim.AppUSIM
		if id.IMPI != "" {
			app = sim.AppISIM
		}
	}
	subID := sub.SubID
	aka := func(ctx context.Context, rand, autn []byte) (res, ck, ik, auts []byte, err error) {
		r, err := h.AKA(ctx, subID, app, rand, autn)
		if errors.Is(err, sim.ErrSyncFailure) {
			return nil, nil, nil, r.AUTS, nil
		}
		if err != nil && app == sim.AppISIM {
			// Fall back to the USIM if the ISIM refuses.
			r, err = h.AKA(ctx, subID, sim.AppUSIM, rand, autn)
			if errors.Is(err, sim.ErrSyncFailure) {
				return nil, nil, nil, r.AUTS, nil
			}
		}
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return r.RES, r.CK, r.IK, nil, nil
	}

	nai := ike.NAI(id.IMSI, sub.MCC, sub.MNC)
	if prof.NAIRealm != "" {
		nai = "0" + id.IMSI + "@" + prof.NAIRealm
	}
	impi, domain := id.IMPI, id.Domain
	if prof.IMSDomain != "" {
		domain = prof.IMSDomain
	}
	if prof.IMPIRealm != "" {
		impi = id.IMSI + "@" + prof.IMPIRealm
	}

	// Tunnel.
	e.phase("connecting", "resolving "+host)
	nc := e.netConfig(cfg)
	ips, err := nc.Resolve(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve ePDG %s: %w", host, err)
	}
	var sess *ike.Session
	var lastErr error
	for _, ip := range ips[:min(len(ips), 3)] {
		e.setStatus(func(s *Status) { s.Step = "ike" })
		e.phase("connecting", "IKEv2 to "+ip.String())
		tr, err := nc.Dial(ctx, ip)
		if err != nil {
			lastErr = err
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		sess, err = ike.Connect(cctx, tr, ike.Config{
			Identity: nai, APN: prof.APN, AKA: aka,
			Proposals: prof.Proposals, IMEI: id.IMEI, DeviceIdentity: prof.DeviceID,
			Progress: func(step string) {
				next := map[string]string{"ike": "auth", "auth": "tunnel"}[step]
				msg := map[string]string{"ike": "EAP-AKA authentication", "auth": "creating IPsec tunnel"}[step]
				e.setStatus(func(s *Status) { s.Step, s.Message = next, msg })
			},
		})
		cancel()
		if err == nil {
			e.setStatus(func(s *Status) { s.EPDGIP = ip.String() })
			break
		}
		lastErr = err
		var ne *ike.NotifyError
		if errors.As(err, &ne) || errors.Is(err, ike.ErrEAPFailure) {
			return err // authoritative rejection: do not try other addresses
		}
	}
	if sess == nil {
		return lastErr
	}
	defer sess.Close()
	info := sess.Info()
	e.setStatus(func(s *Status) {
		s.Phase, s.Message, s.Since, s.Step = "tunnel", "tunnel up", time.Now(), "ims"
		s.InnerIPv4, s.InnerIPv6 = ipStr(info.IPv4), ipStr(info.IPv6)
		s.PCSCF = nil
		for _, p := range info.PCSCF {
			s.PCSCF = append(s.PCSCF, p.String())
		}
		s.IKE, s.ESP = info.IKESuite, info.ESPSuite
	})
	st, err := netio.New(sess, info.IPv4, info.IPv6, slog.Default())
	if err != nil {
		return err
	}
	defer st.Close()

	// IMS. Prefer P-CSCFs from the ePDG; fall back to configured ones.
	pcscfs := info.PCSCF
	if len(pcscfs) == 0 {
		for _, p := range prof.PCSCF {
			if ip := net.ParseIP(p); ip != nil {
				pcscfs = append(pcscfs, ip)
			} else if addrs, err := nc.Resolve(ctx, p); err == nil {
				pcscfs = append(pcscfs, addrs...)
			}
		}
	}
	// On reconnect, try other candidates before the previously used proxy.
	if len(pcscfs) > 1 {
		e.mu.Lock()
		previous := e.lastPCSCF
		e.mu.Unlock()
		candidates := make([]net.IP, 0, len(pcscfs))
		for _, p := range pcscfs {
			if p.String() != previous {
				candidates = append(candidates, p)
			}
		}
		for _, p := range pcscfs {
			if p.String() == previous {
				candidates = append(candidates, p)
			}
		}
		pcscfs = candidates
	}
	e.phase("registering", "IMS registration")
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	ua, err := ims.Start(rctx, ims.Config{
		IMSI: id.IMSI, MCC: sub.MCC, MNC: sub.MNC, IMPI: impi, Domain: domain, IMPU: id.IMPU,
		IMEI: id.IMEI, InstanceID: cfg.InstanceID, AKA: aka, PCSCF: pcscfs, Stack: st, SecAgree: prof.SecAgree, PANI: prof.PANI,
		PreferTCP: prof.PreferTCP, DialURIScheme: prof.DialScheme, UserEqPhone: prof.UserEqPhone,
		SMSCAddress: smsc, CountryCode: prof.CountryCode,
		OnEvent: func(ev ims.Event) {
			e.emit(ev.Kind, ev.Data)
			if ev.Kind == "registration" {
				if r, ok := ev.Data.(ims.Registration); ok {
					e.setStatus(func(s *Status) { s.Reg = &r })
				}
			}
		},
	})
	cancel()
	if err != nil {
		return err
	}
	ua.SetMediaHandlers(func(callID string, f ims.AudioFrame) {
		if e.listener != nil {
			e.listener.OnAudio(callID, f.Frame)
		}
	}, func(callID string, d rune) {
		if e.listener != nil {
			e.listener.OnDTMF(callID, string(d))
		}
	})
	e.mu.Lock()
	e.ua = ua
	ua.SetRawSMS(e.rawSMS)
	e.lastPCSCF = ua.ProxyAddress()
	e.mu.Unlock()
	reg := ua.Registration()
	e.setStatus(func(s *Status) {
		s.Phase, s.Message, s.Since, s.Reg, s.Step = "registered", "WiFi Calling ready", time.Now(), &reg, "ready"
	})
	onRegistered()
	defer func() {
		e.mu.Lock()
		if e.ua == ua {
			e.ua = nil
		}
		e.mu.Unlock()
		c, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		ua.Close(c)
		cancel()
	}()
	select {
	case <-ctx.Done():
		return nil
	case <-kick:
		return errors.New("network changed, reconnecting")
	case <-sess.Done():
		return fmt.Errorf("tunnel lost: %v", sess.Err())
	case <-ua.Done():
		return errors.New("IMS registration lost")
	}
}

func applyOverrides(p *carrier.Profile, c Config) {
	if c.EPDG != "" {
		p.EPDG = c.EPDG
	}
	if c.DialScheme != "" {
		p.DialScheme = c.DialScheme
	}
	if c.SecAgree != nil {
		p.SecAgree = *c.SecAgree
	}
	if c.PreferTCP != nil {
		p.PreferTCP = *c.PreferTCP
	}
	if c.DeviceID != nil {
		p.DeviceID = *c.DeviceID
	}
	if c.CountryCode != "" {
		p.CountryCode = c.CountryCode
	}
	if len(c.Proposals.IKEEnc)+len(c.Proposals.IKEDH)+len(c.Proposals.ESPEnc) > 0 {
		p.Proposals = c.Proposals
	}
	for dst, src := range map[*string]string{&p.IMSDomain: c.IMSDomain, &p.IMPIRealm: c.IMPIRealm,
		&p.NAIRealm: c.NAIRealm, &p.PANI: c.PANI, &p.APN: c.APN} {
		if src != "" {
			*dst = src
		}
	}
	if len(c.PCSCF) > 0 {
		p.PCSCF = c.PCSCF
	}
}

func ipStr(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

func (e *Engine) currentUA() (*ims.UA, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ua == nil {
		return nil, errors.New("WiFi Calling is not registered")
	}
	return e.ua, nil
}

// ------------------------------------------------------------- SMS / calls

// SendSMS sends a text and returns the SMSStatus JSON.
func (e *Engine) SendSMS(to, text string) (string, error) {
	ua, err := e.currentUA()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	st, err := ua.SendSMS(ctx, to, text, true)
	b, _ := json.Marshal(st)
	return string(b), err
}

// Dial starts a call and returns the CallInfo JSON.
func (e *Engine) Dial(number string) (string, error) {
	ua, err := e.currentUA()
	if err != nil {
		return "", err
	}
	c, err := ua.Dial(context.Background(), number)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(c)
	return string(b), nil
}

// Answer accepts an incoming call.
func (e *Engine) Answer(callID string) error {
	ua, err := e.currentUA()
	if err != nil {
		return err
	}
	return ua.Answer(callID)
}

// Hangup ends/declines/cancels a call.
func (e *Engine) Hangup(callID string) error {
	ua, err := e.currentUA()
	if err != nil {
		return err
	}
	return ua.Hangup(context.Background(), callID)
}

// SendDTMF sends one digit.
func (e *Engine) SendDTMF(callID, digit string) error {
	ua, err := e.currentUA()
	if err != nil || digit == "" {
		return err
	}
	return ua.SendDTMF(callID, rune(digit[0]))
}

// PushAudio sends one encoded 20 ms frame.
func (e *Engine) PushAudio(callID string, frame []byte) {
	e.mu.Lock()
	ua := e.ua
	e.mu.Unlock()
	if ua != nil {
		_ = ua.SendAudio(callID, frame)
	}
}

// CallMediaStatsJSON returns RTP receive/transmit counters for one call.
func (e *Engine) CallMediaStatsJSON(callID string) (string, error) {
	ua, err := e.currentUA()
	if err != nil {
		return "", err
	}
	stats, err := ua.CallMediaStats(callID)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(stats)
	return string(b), err
}

// ------------------------------------------------------------------ eSIM

type esimTarget struct {
	SubID int `json:"subId"`
	Slot  int `json:"slot"`
}

// CardsJSON lists UICC slots (eUICC flag, EID if the platform knows it).
func (e *Engine) CardsJSON() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return "", err
	}
	cards, err := h.Cards(ctx)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(cards)
	return string(b), nil
}

func (e *Engine) withCard(targetJSON string, f func(context.Context, *esim.Card) (any, error)) (string, error) {
	var t esimTarget
	if err := json.Unmarshal([]byte(targetJSON), &t); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return "", err
	}
	if t.SubID == 0 {
		t.SubID = -1
	}
	card, err := esim.Open(ctx, h, sim.ChannelTarget{SubID: t.SubID, Slot: t.Slot})
	if err != nil {
		return "", err
	}
	defer card.Close(ctx)
	v, err := f(ctx, card)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(v)
	return string(b), nil
}

// EsimReadJSON reads eUICC info and profiles from a card.
func (e *Engine) EsimReadJSON(targetJSON string) (string, error) {
	return e.withCard(targetJSON, func(ctx context.Context, c *esim.Card) (any, error) {
		info, err := c.ReadInfo(ctx)
		if err != nil {
			return nil, err
		}
		profiles, err := c.Profiles(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"info": info, "profiles": profiles}, nil
	})
}

// EsimProfileOp runs enable | disable | delete | nickname on a profile.
func (e *Engine) EsimProfileOp(targetJSON, op, iccid, arg string) error {
	_, err := e.withCard(targetJSON, func(ctx context.Context, c *esim.Card) (any, error) {
		switch op {
		case "enable":
			return nil, c.Enable(ctx, iccid)
		case "disable":
			return nil, c.Disable(ctx, iccid)
		case "delete":
			return nil, c.Delete(ctx, iccid)
		case "nickname":
			return nil, c.SetNickname(ctx, iccid, arg)
		}
		return nil, fmt.Errorf("unknown operation %q", op)
	})
	return err
}

// ------------------------------------------------------------------ logs

type logHandler struct {
	e     *Engine
	level slog.Level
	attrs []slog.Attr
}

func (l *logHandler) Enabled(_ context.Context, lv slog.Level) bool { return lv >= l.level }
func (l *logHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	for _, a := range l.attrs {
		fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value)
		return true
	})
	l.e.emit("log", map[string]any{"level": r.Level.String(), "time": r.Time, "msg": sb.String()})
	return nil
}
func (l *logHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &logHandler{e: l.e, level: l.level, attrs: append(append([]slog.Attr(nil), l.attrs...), a...)}
}
func (l *logHandler) WithGroup(string) slog.Handler { return l }

// DeviceIMEI returns the IMEI of a logical modem slot ("" if unavailable).
// SGP.22 sends it to the SM-DP+ as part of DeviceInfo during a download.
func (e *Engine) DeviceIMEI(subID, slot int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return ""
	}
	id, err := h.Identity(ctx, subID, slot)
	if err != nil {
		return ""
	}
	return id.IMEI
}

// twoGOnlyMask allows only GSM / GPRS / EDGE (TelephonyManager
// NETWORK_TYPE_BITMASK_*): the phone's own VoLTE (vendor IMS in the modem)
// needs LTE/NR, so restricting one SIM to 2G takes its native IMS
// registration offline while WiFi calling (this app) registers the number.
const twoGOnlyMask int64 = 32771

// allowedTypesPowerReason is ALLOWED_NETWORK_TYPES_REASON_POWER, used by
// nothing else here; the modem applies the intersection of all reasons, so
// the user's own network-type choice in Settings is left untouched.
const allowedTypesPowerReason = 1

// SetCellularBlocked restricts one SIM to 2G (block) or lifts that again,
// returning the allowed-types mask that was in effect before blocking.
// Restoring sets restoreMask (0 = Android's default for that reason) and
// returns the SIM to automatic network selection, undoing the manual
// selection an older build used. The other SIM is not affected. (Powering
// one SIM's radio off also powers off the other on this modem, a manual
// selection of a missing network falls back after 60 s, and the modem
// rejects masks the SIM cannot use, such as CDMA only.)
func (e *Engine) SetCellularBlocked(subID int, block bool, restoreMask int64) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Result string `json:"result"`
	}
	req := map[string]any{"subId": subID, "reason": allowedTypesPowerReason}
	if err := h.Call(ctx, "nettypes", req, &resp); err != nil {
		return 0, err
	}
	before, _ := strconv.ParseInt(resp.Result, 10, 64)
	switch {
	case block:
		req["mask"] = twoGOnlyMask
	case restoreMask > 2 && restoreMask != twoGOnlyMask && restoreMask != 10360:
		req["mask"] = restoreMask
	default:
		req["mask"] = int64(840583) // Android's default for this reason (all types)
	}
	if err := h.Call(ctx, "nettypes", req, &resp); err != nil {
		return before, err
	}
	if !block {
		_ = h.Call(ctx, "netsel", map[string]any{"subId": subID, "block": false}, &resp)
	}
	return before, nil
}

// SetVoiceServiceOverride tells Telephony that voice service for subID is
// available over WiFi (or no longer is). Without it the system dialer refuses
// calls while the SIM has no cellular coverage ("out of service").
func (e *Engine) SetVoiceServiceOverride(subID int, on bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return err
	}
	return h.Call(ctx, "voiceoverride", map[string]any{"subId": subID, "on": on}, nil)
}

// SetOperatorBrand sets (or, with an empty brand, clears) the operator brand
// override Android shows as the carrier name of subID.
func (e *Engine) SetOperatorBrand(subID int, brand string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return err
	}
	var resp struct {
		Result string `json:"result"`
	}
	if err := h.Call(ctx, "brand", map[string]any{"subId": subID, "brand": brand}, &resp); err != nil {
		return err
	}
	if resp.Result != "true" {
		return fmt.Errorf("brand override refused (%s)", resp.Result)
	}
	return nil
}

// SetCrossSimCalling turns the per-SIM "cross-SIM calling" user setting on
// or off and returns whether it was on before.
func (e *Engine) SetCrossSimCalling(subID int, on bool) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return false, err
	}
	var resp struct {
		Result string `json:"result"`
	}
	err = h.Call(ctx, "crosssim", map[string]any{"subId": subID, "on": on}, &resp)
	return resp.Result == "true", err
}

// CellInfoJSON returns a read-only cellular snapshot of a SIM (radio
// technology, band / channel, bandwidths, signal) as JSON.
func (e *Engine) CellInfoJSON(slot, subID int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return "", err
	}
	var resp struct {
		Result string `json:"result"`
	}
	err = h.Call(ctx, "cellinfo", map[string]any{"slot": slot, "subId": subID}, &resp)
	return resp.Result, err
}

// VolteSetting returns the SIM's system "HD calling" (VoLTE) switch.
func (e *Engine) VolteSetting(subID int) (bool, error) {
	return e.boolSetting("volte", map[string]any{"subId": subID})
}

// SetVolteSetting sets the SIM's system "HD calling" (VoLTE) switch and
// returns its previous value.
func (e *Engine) SetVolteSetting(subID int, on bool) (bool, error) {
	return e.boolSetting("volte", map[string]any{"subId": subID, "on": on})
}

// WifiCallingSetting returns the SIM's system "WLAN calling" switch.
func (e *Engine) WifiCallingSetting(subID int) (bool, error) {
	return e.boolSetting("wfc", map[string]any{"subId": subID})
}

// SetWifiCallingSetting sets the SIM's system "WLAN calling" switch and
// returns its previous value.
func (e *Engine) SetWifiCallingSetting(subID int, on bool) (bool, error) {
	return e.boolSetting("wfc", map[string]any{"subId": subID, "on": on})
}

// boolSetting reads (and with "on", sets) a per-SIM boolean system setting
// through the root helper op; it returns the value before any change.
func (e *Engine) boolSetting(op string, req map[string]any) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return false, err
	}
	var resp struct {
		Result string `json:"result"`
	}
	err = h.Call(ctx, op, req, &resp)
	return resp.Result == "true", err
}

// CellularState returns "voice,data,manual,dataRat" for a slot: registration
// states (0 in service, 1 out of service, 2 emergency only, 3 power off),
// whether network selection is manual, and the RIL data radio technology
// (14 LTE, 19 LTE-CA, 20 NR, 18 IWLAN, -1 unknown).
func (e *Engine) CellularState(slot int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return "", err
	}
	var resp struct {
		Result string `json:"result"`
	}
	if err := h.Call(ctx, "svcstate", map[string]any{"slot": slot}, &resp); err != nil {
		return "", err
	}
	return resp.Result, nil
}

// NetworkSelectionMode returns 1 (automatic), 2 (manual) or 0 (unknown) for a SIM.
func (e *Engine) NetworkSelectionMode(subID int) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := e.getHelper(ctx)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Result string `json:"result"`
	}
	if err := h.Call(ctx, "netselmode", map[string]any{"subId": subID}, &resp); err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(resp.Result)
	return n, nil
}

// SetRawSMS hands incoming SMS to the platform (event "sms_raw", answered
// with AckSMS) instead of decoding them; used while the system IMS service
// owns this SIM so the system SMS app receives them.
func (e *Engine) SetRawSMS(on bool) {
	e.mu.Lock()
	e.rawSMS = on
	ua := e.ua
	e.mu.Unlock()
	if ua != nil {
		ua.SetRawSMS(on)
	}
}

// AckSMS answers an "sms_raw" event: RP-ACK when ok, else RP-ERROR(cause).
func (e *Engine) AckSMS(id string, ok bool, cause int) bool {
	ua, err := e.currentUA()
	if err != nil {
		return false
	}
	return ua.AckSMS(id, ok, cause)
}

// SendSMSPDU sends one SMS-SUBMIT TPDU from the platform SMS stack. It
// returns 0 when the SMSC accepted it, otherwise the RP cause; an error
// means it could not be submitted at all.
func (e *Engine) SendSMSPDU(smsc string, tpdu []byte) (int, error) {
	ua, err := e.currentUA()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return ua.SendTPDU(ctx, smsc, tpdu)
}
