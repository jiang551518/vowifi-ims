package ike

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Transport carries IKE and UDP-encapsulated ESP datagrams to one ePDG.
// Port is the remote UDP port (500 or 4500).
type Transport interface {
	Send(port int, b []byte) error
	Recv() (port int, b []byte, err error)
	Remote() net.IP
	Local() net.IP
	Close() error
}

// NetConfig selects how the ePDG is reached. It is shared by the IKE
// transport and the DNS resolver.
type NetConfig struct {
	// Control binds sockets before use (e.g. Android Network.bindSocket).
	Control func(network, address string, c syscall.RawConn) error
	// SOCKS5 proxy "host:port" with UDP ASSOCIATE support. Empty = direct.
	SOCKS5     string
	SOCKSUser  string
	SOCKSPass  string
	DNSMode    string   // "system" (default), "doh", "static"
	DoHServers []string // used by doh mode
	StaticIPs  []string // used by static mode
	PreferIPv6 bool
	Timeout    time.Duration
	// Lookup, when set, replaces the Go resolver in "system" mode (the app
	// resolves on the selected Android network).
	Lookup func(ctx context.Context, host string) ([]net.IP, error)
}

func (c NetConfig) dialer() *net.Dialer {
	t := c.Timeout
	if t == 0 {
		t = 10 * time.Second
	}
	return &net.Dialer{Timeout: t, Control: c.Control}
}

// Resolve looks up the ePDG FQDN according to the DNS mode.
func (c NetConfig) Resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	var ips []net.IP
	var err error
	switch c.DNSMode {
	case "static":
		for _, s := range c.StaticIPs {
			if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil {
				ips = append(ips, ip)
			}
		}
		if len(ips) == 0 {
			err = errors.New("ike: no static ePDG IP configured")
		}
	case "doh":
		ips, err = c.resolveDoH(ctx, host)
	default:
		if c.Lookup != nil {
			ips, err = c.Lookup(ctx, host)
			break
		}
		r := &net.Resolver{PreferGo: false}
		if c.Control != nil {
			r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return c.dialer().DialContext(ctx, network, address)
			}}
		}
		var addrs []net.IPAddr
		addrs, err = r.LookupIPAddr(ctx, host)
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	if err != nil {
		return nil, err
	}
	// Order by family preference.
	var v4, v6 []net.IP
	for _, ip := range ips {
		if ip.To4() != nil {
			v4 = append(v4, ip)
		} else {
			v6 = append(v6, ip)
		}
	}
	if c.PreferIPv6 {
		return append(v6, v4...), nil
	}
	return append(v4, v6...), nil
}

// resolveDoH performs RFC 8484 wire-format queries for A and AAAA.
func (c NetConfig) resolveDoH(ctx context.Context, host string) ([]net.IP, error) {
	servers := c.DoHServers
	if len(servers) == 0 {
		servers = []string{"https://223.5.5.5/dns-query", "https://1.1.1.1/dns-query", "https://8.8.8.8/dns-query"}
	}
	tr := &http.Transport{DialContext: c.dialer().DialContext, TLSHandshakeTimeout: 8 * time.Second}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	var lastErr error
	for _, srv := range servers {
		var ips []net.IP
		for _, qt := range []uint16{1, 28} {
			got, err := dohQuery(ctx, client, srv, host, qt)
			if err != nil {
				lastErr = err
				continue
			}
			ips = append(ips, got...)
		}
		if len(ips) > 0 {
			return ips, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("ike: %s has no address", host)
	}
	return nil, lastErr
}

func dohQuery(ctx context.Context, client *http.Client, server, host string, qtype uint16) ([]net.IP, error) {
	q := []byte{0, 0, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, byte(qtype>>8), byte(qtype), 0, 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server, bytes.NewReader(q))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ike: DoH %s status %d", server, resp.StatusCode)
	}
	return parseDNSAnswers(b, qtype)
}

func parseDNSAnswers(b []byte, qtype uint16) ([]net.IP, error) {
	if len(b) < 12 {
		return nil, errors.New("dns: short reply")
	}
	if rc := b[3] & 0xf; rc != 0 {
		return nil, fmt.Errorf("dns: rcode %d", rc)
	}
	qd, an := int(binary.BigEndian.Uint16(b[4:])), int(binary.BigEndian.Uint16(b[6:]))
	off := 12
	skipName := func() error {
		for off < len(b) {
			l := int(b[off])
			switch {
			case l == 0:
				off++
				return nil
			case l&0xC0 == 0xC0:
				off += 2
				return nil
			default:
				off += 1 + l
			}
		}
		return errors.New("dns: bad name")
	}
	for i := 0; i < qd; i++ {
		if err := skipName(); err != nil {
			return nil, err
		}
		off += 4
	}
	var ips []net.IP
	for i := 0; i < an; i++ {
		if err := skipName(); err != nil {
			return nil, err
		}
		if off+10 > len(b) {
			return nil, errors.New("dns: truncated answer")
		}
		t := binary.BigEndian.Uint16(b[off:])
		rdl := int(binary.BigEndian.Uint16(b[off+8:]))
		off += 10
		if off+rdl > len(b) {
			return nil, errors.New("dns: truncated rdata")
		}
		if t == qtype && (rdl == 4 || rdl == 16) {
			ips = append(ips, net.IP(append([]byte(nil), b[off:off+rdl]...)))
		}
		off += rdl
	}
	return ips, nil
}

// Dial opens a transport to the ePDG at ip.
func (c NetConfig) Dial(ctx context.Context, ip net.IP) (Transport, error) {
	if c.SOCKS5 != "" {
		return dialSOCKS5(ctx, c, ip)
	}
	return dialDirect(ctx, c, ip)
}

// ------------------------------------------------------------------ direct

type directTransport struct {
	conn   net.PacketConn
	remote net.IP
	local  net.IP
}

func dialDirect(ctx context.Context, c NetConfig, ip net.IP) (Transport, error) {
	network := "udp4"
	laddr := "0.0.0.0:0"
	if ip.To4() == nil {
		network, laddr = "udp6", "[::]:0"
	}
	lc := net.ListenConfig{Control: c.Control}
	pc, err := lc.ListenPacket(ctx, network, laddr)
	if err != nil {
		return nil, err
	}
	t := &directTransport{conn: pc, remote: ip}
	// Learn the local address that routes to the ePDG (for NAT detection
	// and logging only).
	if probe, err := c.dialer().DialContext(ctx, network, net.JoinHostPort(ip.String(), "4500")); err == nil {
		t.local = probe.LocalAddr().(*net.UDPAddr).IP
		probe.Close()
	}
	return t, nil
}

func (t *directTransport) Send(port int, b []byte) error {
	_, err := t.conn.WriteTo(b, &net.UDPAddr{IP: t.remote, Port: port})
	return err
}

func (t *directTransport) Recv() (int, []byte, error) {
	buf := make([]byte, 65536)
	for {
		n, addr, err := t.conn.ReadFrom(buf)
		if err != nil {
			return 0, nil, err
		}
		ua, ok := addr.(*net.UDPAddr)
		if !ok || !ua.IP.Equal(t.remote) {
			continue
		}
		return ua.Port, append([]byte(nil), buf[:n]...), nil
	}
}

func (t *directTransport) Remote() net.IP { return t.remote }
func (t *directTransport) Local() net.IP  { return t.local }
func (t *directTransport) Close() error   { return t.conn.Close() }

// ------------------------------------------------------------------ SOCKS5

type socksTransport struct {
	ctrl   net.Conn
	conn   net.PacketConn
	relay  *net.UDPAddr
	remote net.IP
	mu     sync.Mutex
}

func dialSOCKS5(ctx context.Context, c NetConfig, ip net.IP) (Transport, error) {
	ctrl, err := c.dialer().DialContext(ctx, "tcp", c.SOCKS5)
	if err != nil {
		return nil, fmt.Errorf("socks5: connect: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			ctrl.Close()
		}
	}()
	_ = ctrl.SetDeadline(time.Now().Add(10 * time.Second))
	methods := []byte{5, 1, 0}
	if c.SOCKSUser != "" {
		methods = []byte{5, 2, 0, 2}
	}
	if _, err := ctrl.Write(methods); err != nil {
		return nil, err
	}
	rep := make([]byte, 2)
	if _, err := io.ReadFull(ctrl, rep); err != nil {
		return nil, fmt.Errorf("socks5: greeting: %w", err)
	}
	switch rep[1] {
	case 0:
	case 2:
		u, p := []byte(c.SOCKSUser), []byte(c.SOCKSPass)
		msg := append([]byte{1, byte(len(u))}, u...)
		msg = append(append(msg, byte(len(p))), p...)
		if _, err := ctrl.Write(msg); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(ctrl, rep); err != nil || rep[1] != 0 {
			return nil, errors.New("socks5: authentication rejected")
		}
	default:
		return nil, errors.New("socks5: no acceptable auth method")
	}
	// UDP ASSOCIATE with an unspecified client address.
	if _, err := ctrl.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return nil, err
	}
	relay, err := readSOCKSReply(ctrl)
	if err != nil {
		return nil, err
	}
	if relay.IP.IsUnspecified() {
		relay.IP = ctrl.RemoteAddr().(*net.TCPAddr).IP
	}
	_ = ctrl.SetDeadline(time.Time{})
	lc := net.ListenConfig{Control: c.Control}
	network := "udp4"
	if relay.IP.To4() == nil {
		network = "udp6"
	}
	pc, err := lc.ListenPacket(ctx, network, ":0")
	if err != nil {
		return nil, err
	}
	ok = true
	t := &socksTransport{ctrl: ctrl, conn: pc, relay: relay, remote: ip}
	// The association lives as long as the TCP control connection.
	go func() {
		_, _ = io.Copy(io.Discard, ctrl)
		pc.Close()
	}()
	return t, nil
}

func readSOCKSReply(r io.Reader) (*net.UDPAddr, error) {
	h := make([]byte, 4)
	if _, err := io.ReadFull(r, h); err != nil {
		return nil, fmt.Errorf("socks5: reply: %w", err)
	}
	if h[1] != 0 {
		return nil, fmt.Errorf("socks5: UDP ASSOCIATE refused (code %d)", h[1])
	}
	var ip net.IP
	switch h[3] {
	case 1:
		ip = make(net.IP, 4)
	case 4:
		ip = make(net.IP, 16)
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(r, l); err != nil {
			return nil, err
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(r, name); err != nil {
			return nil, err
		}
		p := make([]byte, 2)
		if _, err := io.ReadFull(r, p); err != nil {
			return nil, err
		}
		addrs, err := net.LookupIP(string(name))
		if err != nil || len(addrs) == 0 {
			return nil, fmt.Errorf("socks5: cannot resolve relay %s", name)
		}
		return &net.UDPAddr{IP: addrs[0], Port: int(binary.BigEndian.Uint16(p))}, nil
	default:
		return nil, errors.New("socks5: bad address type")
	}
	if _, err := io.ReadFull(r, ip); err != nil {
		return nil, err
	}
	p := make([]byte, 2)
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: ip, Port: int(binary.BigEndian.Uint16(p))}, nil
}

func (t *socksTransport) Send(port int, b []byte) error {
	var h []byte
	if v4 := t.remote.To4(); v4 != nil {
		h = append([]byte{0, 0, 0, 1}, v4...)
	} else {
		h = append([]byte{0, 0, 0, 4}, t.remote.To16()...)
	}
	h = append(h, byte(port>>8), byte(port))
	_, err := t.conn.WriteTo(append(h, b...), t.relay)
	return err
}

func (t *socksTransport) Recv() (int, []byte, error) {
	buf := make([]byte, 65536)
	for {
		n, _, err := t.conn.ReadFrom(buf)
		if err != nil {
			return 0, nil, err
		}
		b := buf[:n]
		if len(b) < 10 || b[2] != 0 {
			continue // fragmented or malformed
		}
		var off int
		var src net.IP
		switch b[3] {
		case 1:
			src, off = net.IP(b[4:8]), 8
		case 4:
			if len(b) < 22 {
				continue
			}
			src, off = net.IP(b[4:20]), 20
		default:
			continue
		}
		if !src.Equal(t.remote) {
			continue
		}
		port := int(binary.BigEndian.Uint16(b[off:]))
		return port, append([]byte(nil), b[off+2:]...), nil
	}
}

func (t *socksTransport) Remote() net.IP { return t.remote }
func (t *socksTransport) Local() net.IP  { return nil }
func (t *socksTransport) Close() error {
	t.conn.Close()
	return t.ctrl.Close()
}

// EPDGHost returns the standard ePDG FQDN for a PLMN (TS 23.003 §19.4.2.9).
func EPDGHost(mcc, mnc string) string {
	return "epdg.epc.mnc" + pad3(mnc) + ".mcc" + mcc + ".pub.3gppnetwork.org"
}

// NAI returns the permanent identity used for SWu (TS 23.003 §19.3.2).
func NAI(imsi, mcc, mnc string) string {
	return "0" + imsi + "@nai.epc.mnc" + pad3(mnc) + ".mcc" + mcc + ".3gppnetwork.org"
}

func pad3(mnc string) string {
	if len(mnc) == 2 {
		return "0" + mnc
	}
	return mnc
}
