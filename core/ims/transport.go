package ims

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"

	"vowifi/netio"
)

// Timer values from RFC 3261.
const (
	T1 = 500 * time.Millisecond
	T2 = 4 * time.Second
)

// maxUDP is the size above which requests go over TCP (RFC 3261 §18.1.1).
const maxUDP = 1200

// origin records where an incoming message came from, for replies.
type origin struct {
	udp   *gonet.UDPConn
	raddr net.Addr
	tcp   *tcpConn
}

type tcpConn struct {
	c     net.Conn
	wmu   sync.Mutex
	local int
}

func (t *tcpConn) write(b []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_ = t.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := t.c.Write(b)
	return err
}

// Transport sends and receives SIP over the tunnel's inner stack.
type Transport struct {
	st    *netio.Stack
	log   *slog.Logger
	Local net.IP
	PCSCF net.IP

	// Local ports: PortC sends requests and receives their responses, PortS
	// receives requests (sec-agree protected server port).
	PortC, PortS int
	// Remote ports of the P-CSCF: 5060 before sec-agree.
	RemoteC, RemoteS int
	PreferTCP        bool
	// Protected is set once sec-agree SAs exist; before that, TCP uses an
	// ephemeral source port.
	Protected bool

	mu        sync.Mutex
	udp       map[int]*gonet.UDPConn
	listeners []net.Listener
	clientTCP *tcpConn
	pending   map[string]*clientTx
	handler   func(*Message)
	closed    bool
	done      chan struct{}
	failures  chan error
}

type clientTx struct {
	ch  chan *Message
	inv bool
}

// NewTransport binds UDP and TCP on both local ports.
func NewTransport(st *netio.Stack, pcscf net.IP, portC, portS int, log *slog.Logger) (*Transport, error) {
	t := &Transport{st: st, log: log, Local: st.LocalFor(pcscf), PCSCF: pcscf, PortC: portC, PortS: portS,
		RemoteC: 5060, RemoteS: 5060, udp: map[int]*gonet.UDPConn{}, pending: map[string]*clientTx{}, done: make(chan struct{}), failures: make(chan error, 1)}
	if t.Local == nil {
		return nil, netio.ErrNoAddress
	}
	// UDP on both ports; TCP listens only on the server port because the
	// client port is used to originate the outgoing TCP connection.
	for _, p := range []int{portC, portS} {
		if err := t.bind(p, p == portS); err != nil {
			t.Close()
			return nil, err
		}
	}
	go t.keepAliveLoop()
	return t, nil
}

func (t *Transport) bind(port int, listenTCP bool) error {
	u, err := t.st.ListenUDP(t.Local, port)
	if err != nil {
		return fmt.Errorf("sip: bind udp %d: %w", port, err)
	}
	t.mu.Lock()
	t.udp[port] = u
	t.mu.Unlock()
	go t.udpLoop(u)
	if listenTCP {
		l, err := t.st.ListenTCP(t.Local, port)
		if err != nil {
			return fmt.Errorf("sip: bind tcp %d: %w", port, err)
		}
		t.mu.Lock()
		t.listeners = append(t.listeners, l)
		t.mu.Unlock()
		go t.acceptLoop(l, port)
	}
	return nil
}

// SetHandler installs the receiver of incoming requests.
func (t *Transport) SetHandler(h func(*Message)) {
	t.mu.Lock()
	t.handler = h
	t.mu.Unlock()
}

// Close releases all sockets.
func (t *Transport) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	close(t.done)
	for _, u := range t.udp {
		u.Close()
	}
	for _, l := range t.listeners {
		l.Close()
	}
	if t.clientTCP != nil {
		t.clientTCP.c.Close()
	}
	for k, tx := range t.pending {
		close(tx.ch)
		delete(t.pending, k)
	}
	t.mu.Unlock()
}

// HostPort formats the local address for Via/Contact.
func (t *Transport) HostPort(port int) string {
	return net.JoinHostPort(t.Local.String(), strconv.Itoa(port))
}

func (t *Transport) udpLoop(u *gonet.UDPConn) {
	buf := make([]byte, 65536)
	for {
		n, addr, err := u.ReadFrom(buf)
		if err != nil {
			return
		}
		m, err := Parse(append([]byte(nil), buf[:n]...))
		if err != nil {
			if n > 4 {
				t.log.Debug("sip: unparsable datagram", "err", err)
			}
			continue
		}
		t.dispatch(m, origin{udp: u, raddr: addr})
	}
}

func (t *Transport) acceptLoop(l net.Listener, port int) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go t.readTCP(&tcpConn{c: c, local: port})
	}
}

func (t *Transport) readTCP(tc *tcpConn) {
	r := bufio.NewReaderSize(tc.c, 65536)
	for {
		m, err := ReadStream(r)
		if err != nil {
			tc.c.Close()
			t.mu.Lock()
			if t.clientTCP == tc {
				t.clientTCP = nil
				if !t.closed {
					t.reportFailure(fmt.Errorf("sip: TCP disconnected: %w", err))
				}
			}
			t.mu.Unlock()
			return
		}
		t.dispatch(m, origin{tcp: tc})
	}
}

// sipTrace dumps every SIP message to stderr (debug CLI only: VW_SIPTRACE=1).
var sipTrace = os.Getenv("VW_SIPTRACE") == "1"

func (t *Transport) dispatch(m *Message, o origin) {
	if sipTrace {
		fmt.Fprintf(os.Stderr, "\n<<<<<<\n%s\n", m.Bytes())
	}
	if m.IsRequest() {
		m.origin = &o
		t.mu.Lock()
		h := t.handler
		t.mu.Unlock()
		if h != nil {
			go h(m)
		}
		return
	}
	branch, _ := Param(m.Get("Via"), "branch")
	t.mu.Lock()
	defer t.mu.Unlock()
	tx := t.pending[branch]
	if tx != nil {
		select {
		case tx.ch <- m:
		default:
		}
		// Only REGISTER failures mean the P-CSCF path is broken. 408/5xx on
		// INVITE, PRACK, MESSAGE... usually come from the far end or an AS
		// (e.g. 503 when the callee is unreachable) and must not tear down
		// the registration together with every call.
		if (m.StatusCode == 408 || (m.StatusCode >= 500 && m.StatusCode <= 599)) &&
			strings.HasSuffix(strings.TrimSpace(m.Get("CSeq")), "REGISTER") {
			t.reportFailure(fmt.Errorf("sip: P-CSCF returned %d", m.StatusCode))
		}
	}
}

// ------------------------------------------------------------- sending

func (t *Transport) sendUDP(b []byte, from, to int) error {
	t.mu.Lock()
	u := t.udp[from]
	t.mu.Unlock()
	if u == nil {
		return errors.New("sip: socket closed")
	}
	_, err := u.WriteTo(b, &net.UDPAddr{IP: t.PCSCF, Port: to})
	return err
}

func (t *Transport) tcpClient(ctx context.Context) (*tcpConn, error) {
	t.mu.Lock()
	if c := t.clientTCP; c != nil {
		t.mu.Unlock()
		return c, nil
	}
	t.mu.Unlock()
	lport := 0
	if t.Protected {
		lport = t.PortC
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := t.st.DialTCP(dctx, t.PCSCF, t.RemoteS, lport)
	if err != nil {
		return nil, fmt.Errorf("sip: tcp connect: %w", err)
	}
	tc := &tcpConn{c: c, local: c.LocalAddr().(*net.TCPAddr).Port}
	t.mu.Lock()
	t.clientTCP = tc
	t.mu.Unlock()
	go t.readTCP(tc)
	return tc, nil
}

// ResetTCP drops the client TCP connection (e.g. after sec-agree changes).
func (t *Transport) ResetTCP() {
	t.mu.Lock()
	c := t.clientTCP
	t.clientTCP = nil
	t.mu.Unlock()
	if c != nil {
		c.c.Close()
	}
}

// Request sends req through a client transaction and returns the final
// response. Provisional responses are passed to onProvisional.
func (t *Transport) Request(ctx context.Context, req *Message, onProvisional func(*Message)) (*Message, error) {
	useTCP := t.PreferTCP || len(req.Bytes()) > maxUDP
	proto := "UDP"
	if useTCP {
		proto = "TCP"
	}
	branch := newBranch()
	viaPort := t.PortC
	var conn *tcpConn
	if useTCP {
		var err error
		if conn, err = t.tcpClient(ctx); err != nil {
			return nil, err
		}
		viaPort = conn.local
	}
	req.Del("Via")
	via := fmt.Sprintf("SIP/2.0/%s %s;branch=%s;rport", proto, t.HostPort(viaPort), branch)
	req.headers = append([]header{{"Via", via}}, req.headers...)
	tx := &clientTx{ch: make(chan *Message, 8), inv: req.Method == "INVITE"}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errors.New("sip: transport closed")
	}
	t.pending[branch] = tx
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, branch)
		t.mu.Unlock()
	}()
	raw := req.Bytes()
	t.log.Debug("sip: send", "method", req.Method, "proto", proto, "bytes", len(raw))
	if sipTrace {
		fmt.Fprintf(os.Stderr, "\n>>>>>> %s\n%s\n", proto, raw)
	}
	send := func() error {
		if useTCP {
			return conn.write(raw)
		}
		return t.sendUDP(raw, t.PortC, t.RemoteS)
	}
	if err := send(); err != nil {
		return nil, err
	}
	interval := T1
	deadline := time.Now().Add(64 * T1)
	gotProvisional := false
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-tx.ch:
			if !ok {
				return nil, errors.New("sip: transport closed")
			}
			if m.StatusCode < 200 {
				gotProvisional = true
				if onProvisional != nil {
					onProvisional(m)
				}
				continue
			}
			return m, nil
		case <-timer.C:
			if tx.inv && gotProvisional {
				// INVITE: wait for the final response without retransmitting.
				timer.Reset(time.Second)
				continue
			}
			if time.Now().After(deadline) {
				err := fmt.Errorf("sip: %s timed out", req.Method)
				t.reportFailure(err)
				return nil, err
			}
			if !useTCP {
				_ = send()
			}
			if interval *= 2; interval > T2 && !tx.inv {
				interval = T2
			}
			timer.Reset(interval)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// SendACK sends an ACK (no transaction, no response expected).
func (t *Transport) SendACK(ack *Message) error {
	ack.Del("Via")
	via := fmt.Sprintf("SIP/2.0/UDP %s;branch=%s;rport", t.HostPort(t.PortC), newBranch())
	ack.headers = append([]header{{"Via", via}}, ack.headers...)
	raw := ack.Bytes()
	t.mu.Lock()
	c := t.clientTCP
	t.mu.Unlock()
	if t.PreferTCP && c != nil {
		ack.Set("Via", strings.Replace(via, "/UDP ", "/TCP ", 1))
		return c.write(ack.Bytes())
	}
	return t.sendUDP(raw, t.PortC, t.RemoteS)
}

// Reply sends a response to an incoming request over the path it came from.
func (t *Transport) Reply(req *Message, resp *Message) error {
	raw := resp.Bytes()
	o := req.origin
	if o == nil {
		return errors.New("sip: request without origin")
	}
	if o.tcp != nil {
		return o.tcp.write(raw)
	}
	// Respond to the Via sent-by port (rport if present).
	port := t.RemoteC
	if ua, ok := o.raddr.(*net.UDPAddr); ok {
		port = ua.Port
	}
	_, err := o.udp.WriteTo(raw, &net.UDPAddr{IP: t.PCSCF, Port: port})
	return err
}

// NewResponse builds a response mirroring the dialog headers of req.
func NewResponse(req *Message, code int, reason string) *Message {
	r := &Message{StatusCode: code, Reason: reason}
	for _, v := range req.Values("Via") {
		r.Add("Via", v)
	}
	for _, v := range req.Values("Record-Route") {
		r.Add("Record-Route", v)
	}
	r.Add("From", req.Get("From"))
	to := req.Get("To")
	if _, ok := Param(to, "tag"); !ok && code > 100 {
		to += ";tag=" + newTag()
	}
	r.Add("To", to)
	r.Add("Call-ID", req.Get("Call-ID"))
	r.Add("CSeq", req.Get("CSeq"))
	return r
}

// SendRaw sends a request that already carries its Via (ACK for non-2xx
// responses and CANCEL reuse the INVITE's branch) without a transaction.
func (t *Transport) SendRaw(m *Message) error {
	raw := m.Bytes()
	if strings.Contains(m.Get("Via"), "/TCP ") {
		t.mu.Lock()
		c := t.clientTCP
		t.mu.Unlock()
		if c != nil {
			return c.write(raw)
		}
	}
	return t.sendUDP(raw, t.PortC, t.RemoteS)
}

// reportFailure never blocks a reader or a transaction dispatcher.
func (t *Transport) reportFailure(err error) {
	select {
	case t.failures <- err:
	default:
	}
}

// Keep an established SIP TCP flow alive without opening a new connection.
func (t *Transport) keepAliveLoop() {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-tick.C:
			t.mu.Lock()
			c := t.clientTCP
			t.mu.Unlock()
			if c != nil {
				if err := c.write([]byte("\r\n\r\n")); err != nil {
					t.mu.Lock()
					if !t.closed && t.clientTCP == c {
						t.reportFailure(fmt.Errorf("sip: TCP keepalive: %w", err))
					}
					t.mu.Unlock()
				}
			}
		}
	}
}
