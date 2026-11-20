// Package netio runs a userspace TCP/IP stack (gVisor netstack) on top of
// the SWu ESP tunnel, and applies IMS sec-agree transport-mode ESP
// (TS 33.203) to selected flows before they enter the tunnel.
package netio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"gvisor.dev/gvisor/pkg/waiter"

	"vowifi/ike"
)

// MTU of the inner interface. 1280 is the IPv6 minimum and leaves room for
// ESP-in-UDP and proxy overhead on any path.
const MTU = 1280

// Tunnel is the minimal tunnel interface netio needs.
type Tunnel interface {
	SendPacket([]byte) error
	SetPacketHandler(func([]byte))
}

// Stack is a userspace network stack bound to the tunnel's inner addresses.
type Stack struct {
	s      *stack.Stack
	ep     *channel.Endpoint
	tun    Tunnel
	IPv4   net.IP
	IPv6   net.IP
	log    *slog.Logger
	cancel context.CancelFunc

	mu       sync.RWMutex
	outRules []TransportRule
	inSAs    map[uint32]*ike.ESPSA
}

// TransportRule protects packets from the local address to Remote whose
// ports match with transport-mode ESP using SA.
type TransportRule struct {
	Remote  net.IP
	SrcPort uint16 // 0 = any
	DstPort uint16 // 0 = any
	SA      *ike.ESPSA
}

// New builds the stack and starts pumping packets.
func New(tun Tunnel, v4, v6 net.IP, log *slog.Logger) (*Stack, error) {
	if log == nil {
		log = slog.Default()
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	ep := channel.New(1024, MTU, "")
	if err := s.CreateNIC(1, ep); err != nil {
		return nil, fmt.Errorf("netio: create NIC: %v", err)
	}
	var routes []tcpip.Route
	if v4 != nil {
		if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddrFromSlice(v4.To4()).WithPrefix()}, stack.AddressProperties{}); err != nil {
			return nil, fmt.Errorf("netio: add IPv4: %v", err)
		}
		routes = append(routes, tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: 1})
	}
	if v6 != nil {
		if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv6.ProtocolNumber,
			AddressWithPrefix: tcpip.AddrFromSlice(v6.To16()).WithPrefix()}, stack.AddressProperties{}); err != nil {
			return nil, fmt.Errorf("netio: add IPv6: %v", err)
		}
		routes = append(routes, tcpip.Route{Destination: header.IPv6EmptySubnet, NIC: 1})
	}
	s.SetRouteTable(routes)
	// Moderate TCP buffers; SIP traffic is small.
	sack := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)

	ctx, cancel := context.WithCancel(context.Background())
	st := &Stack{s: s, ep: ep, tun: tun, IPv4: v4, IPv6: v6, log: log, cancel: cancel, inSAs: map[uint32]*ike.ESPSA{}}
	tun.SetPacketHandler(st.inbound)
	go st.outboundLoop(ctx)
	return st, nil
}

// Close stops the stack.
func (st *Stack) Close() {
	st.cancel()
	st.tun.SetPacketHandler(nil)
	st.ep.Close()
	st.s.Close()
}

func (st *Stack) outboundLoop(ctx context.Context) {
	for {
		pkt := st.ep.ReadContext(ctx)
		if pkt == nil {
			return
		}
		v := pkt.ToView()
		b := append([]byte(nil), v.AsSlice()...)
		v.Release()
		pkt.DecRef()
		b = st.protectOutbound(b)
		if b == nil {
			continue
		}
		if err := st.tun.SendPacket(b); err != nil {
			st.log.Debug("netio: tunnel send failed", "err", err)
		}
	}
}

func (st *Stack) inbound(b []byte) {
	if len(b) < 1 {
		return
	}
	b = st.unprotectInbound(b)
	if b == nil {
		return
	}
	proto := ipv4.ProtocolNumber
	if b[0]>>4 == 6 {
		proto = ipv6.ProtocolNumber
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(b)})
	st.ep.InjectInbound(proto, pkt)
	pkt.DecRef()
}

// ------------------------------------------------- sec-agree transport ESP

// SetTransportSAs installs the IMS IPsec SAs. Passing nil clears them.
func (st *Stack) SetTransportSAs(out []TransportRule, in []*ike.ESPSA) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.outRules = out
	st.inSAs = map[uint32]*ike.ESPSA{}
	for _, sa := range in {
		st.inSAs[sa.SPI] = sa
	}
}

// AddTransportSAs adds SAs without removing existing ones (used during
// re-registration, when old and new SAs overlap).
func (st *Stack) AddTransportSAs(out []TransportRule, in []*ike.ESPSA) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.outRules = append(out, st.outRules...)
	for _, sa := range in {
		st.inSAs[sa.SPI] = sa
	}
}

// parseL4 returns the IP header length, protocol and ports of a packet
// without extension headers (fragments are left untouched).
func parseL4(b []byte) (hl int, proto uint8, src, dst net.IP, sport, dport uint16, ok bool) {
	switch b[0] >> 4 {
	case 4:
		if len(b) < 20 {
			return
		}
		hl = int(b[0]&0xf) * 4
		if binary.BigEndian.Uint16(b[6:])&0x3fff != 0 { // fragmented
			return
		}
		proto, src, dst = b[9], net.IP(b[12:16]), net.IP(b[16:20])
	case 6:
		if len(b) < 40 {
			return
		}
		hl, proto, src, dst = 40, b[6], net.IP(b[8:24]), net.IP(b[24:40])
	default:
		return
	}
	if (proto == 6 || proto == 17) && len(b) >= hl+4 {
		sport, dport = binary.BigEndian.Uint16(b[hl:]), binary.BigEndian.Uint16(b[hl+2:])
	}
	ok = true
	return
}

func (st *Stack) protectOutbound(b []byte) []byte {
	st.mu.RLock()
	rules := st.outRules
	st.mu.RUnlock()
	if len(rules) == 0 {
		return b
	}
	hl, proto, _, dst, sport, dport, ok := parseL4(b)
	if !ok || (proto != 6 && proto != 17) {
		return b
	}
	for _, r := range rules {
		if !r.Remote.Equal(dst) || (r.SrcPort != 0 && r.SrcPort != sport) || (r.DstPort != 0 && r.DstPort != dport) {
			continue
		}
		esp, err := r.SA.Seal(b[hl:], int(proto))
		if err != nil {
			st.log.Warn("netio: transport ESP seal failed", "err", err)
			return nil
		}
		out := append(append([]byte(nil), b[:hl]...), esp...)
		setProtoAndLength(out, hl, 50)
		return out
	}
	return b
}

func (st *Stack) unprotectInbound(b []byte) []byte {
	hl, proto, _, _, _, _, ok := parseL4(b)
	if !ok || proto != 50 {
		return b
	}
	if len(b) < hl+8 {
		return nil
	}
	spi := binary.BigEndian.Uint32(b[hl:])
	st.mu.RLock()
	sa := st.inSAs[spi]
	st.mu.RUnlock()
	if sa == nil {
		st.log.Debug("netio: transport ESP for unknown SPI", "spi", spi)
		return nil
	}
	payload, nh, err := sa.Open(b[hl:])
	if err != nil {
		st.log.Debug("netio: transport ESP open failed", "err", err)
		return nil
	}
	out := append(append([]byte(nil), b[:hl]...), payload...)
	setProtoAndLength(out, hl, uint8(nh))
	return out
}

func setProtoAndLength(b []byte, hl int, proto uint8) {
	if b[0]>>4 == 4 {
		b[9] = proto
		binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
		b[10], b[11] = 0, 0
		var sum uint32
		for i := 0; i < hl; i += 2 {
			sum += uint32(binary.BigEndian.Uint16(b[i:]))
		}
		for sum>>16 != 0 {
			sum = sum&0xffff + sum>>16
		}
		binary.BigEndian.PutUint16(b[10:], ^uint16(sum))
		return
	}
	b[6] = proto
	binary.BigEndian.PutUint16(b[4:], uint16(len(b)-40))
}

// ------------------------------------------------------------ sockets

func (st *Stack) fullAddr(ip net.IP, port int) (tcpip.FullAddress, tcpip.NetworkProtocolNumber) {
	if v4 := ip.To4(); v4 != nil {
		return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(v4), Port: uint16(port)}, ipv4.ProtocolNumber
	}
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(ip.To16()), Port: uint16(port)}, ipv6.ProtocolNumber
}

// LocalFor returns the inner address of the family matching remote.
func (st *Stack) LocalFor(remote net.IP) net.IP {
	if remote.To4() != nil {
		return st.IPv4
	}
	return st.IPv6
}

// ListenUDP binds a UDP socket on the inner address.
func (st *Stack) ListenUDP(local net.IP, port int) (*gonet.UDPConn, error) {
	la, proto := st.fullAddr(local, port)
	return gonet.DialUDP(st.s, &la, nil, proto)
}

// DialTCP connects to remote, optionally from a fixed local port. A fixed
// port is bound with SO_REUSEADDR so that it can be reused while an earlier
// connection from it lingers in TIME_WAIT (sec-agree pins the client port).
func (st *Stack) DialTCP(ctx context.Context, remote net.IP, rport, lport int) (*gonet.TCPConn, error) {
	ra, proto := st.fullAddr(remote, rport)
	if lport == 0 {
		return gonet.DialContextTCP(ctx, st.s, ra, proto)
	}
	la, _ := st.fullAddr(st.LocalFor(remote), lport)
	var wq waiter.Queue
	ep, terr := st.s.NewEndpoint(tcp.ProtocolNumber, proto, &wq)
	if terr != nil {
		return nil, errors.New(terr.String())
	}
	ep.SocketOptions().SetReuseAddress(true)
	entry, notify := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)
	if terr := ep.Bind(la); terr != nil {
		ep.Close()
		return nil, fmt.Errorf("netio: bind %d: %s", lport, terr)
	}
	terr = ep.Connect(ra)
	if _, ok := terr.(*tcpip.ErrConnectStarted); ok {
		select {
		case <-ctx.Done():
			ep.Close()
			return nil, ctx.Err()
		case <-notify:
		}
		terr = ep.LastError()
	}
	if terr != nil {
		ep.Close()
		return nil, fmt.Errorf("netio: connect: %s", terr)
	}
	return gonet.NewTCPConn(&wq, ep), nil
}

// ListenTCP listens on the inner address.
func (st *Stack) ListenTCP(local net.IP, port int) (*gonet.TCPListener, error) {
	la, proto := st.fullAddr(local, port)
	return gonet.ListenTCP(st.s, la, proto)
}

// ErrNoAddress is returned when the tunnel has no address of a family.
var ErrNoAddress = errors.New("netio: no inner address for that family")
