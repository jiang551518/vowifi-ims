// Package ike implements an IKEv2 initiator (RFC 7296) for the 3GPP SWu
// interface (TS 24.302 / TS 33.402): EAP-AKA authentication against an
// ePDG, configuration payload for inner addresses and P-CSCF discovery, and a
// single tunnel-mode ESP child SA carried in UDP (RFC 3948).
package ike

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Exchange types.
const (
	ExchangeSAInit        = 34
	ExchangeAuth          = 35
	ExchangeCreateChild   = 36
	ExchangeInformational = 37
)

// Header flags.
const (
	FlagInitiator = 0x08
	FlagResponse  = 0x20
)

// Payload types.
const (
	PayloadNone    = 0
	PayloadSA      = 33
	PayloadKE      = 34
	PayloadIDi     = 35
	PayloadIDr     = 36
	PayloadCERT    = 37
	PayloadCERTREQ = 38
	PayloadAUTH    = 39
	PayloadNonce   = 40
	PayloadNotify  = 41
	PayloadDelete  = 42
	PayloadVendor  = 43
	PayloadTSi     = 44
	PayloadTSr     = 45
	PayloadSK      = 46
	PayloadCP      = 47
	PayloadEAP     = 48
	PayloadSKF     = 53
)

// Notify message types used here.
const (
	NotifyUnsupportedCritical    = 1
	NotifyInvalidSyntax          = 7
	NotifyNoProposalChosen       = 14
	NotifyInvalidKEPayload       = 17
	NotifyAuthenticationFailed   = 24
	NotifySinglePairRequired     = 34
	NotifyNoAdditionalSAs        = 35
	NotifyInternalAddressFailure = 36
	NotifyFailedCPRequired       = 37
	NotifyTSUnacceptable         = 38
	NotifyTemporaryFailure       = 43
	NotifyChildSANotFound        = 44

	NotifyInitialContact          = 16384
	NotifySetWindowSize           = 16385
	NotifyNATDetectionSourceIP    = 16388
	NotifyNATDetectionDestIP      = 16389
	NotifyCookie                  = 16390
	NotifyUseTransportMode        = 16391
	NotifyRekeySA                 = 16393
	NotifyMOBIKESupported         = 16396
	NotifyEAPOnlyAuthentication   = 16417
	NotifyFragmentationSupported  = 16430
	NotifySignatureHashAlgorithms = 16431

	// 3GPP private notify types (TS 24.302 §8.1.2).
	NotifyDeviceIdentity = 41101
	NotifyBackoffTimer   = 41041
)

// Errors returned for IKE level failures.
var (
	ErrShort = errors.New("ike: message too short")
)

// Header is the fixed IKE header.
type Header struct {
	SPIi, SPIr  uint64
	NextPayload uint8
	Exchange    uint8
	Flags       uint8
	MessageID   uint32
	Length      uint32
}

const headerLen = 28

func (h Header) marshal(b []byte) {
	binary.BigEndian.PutUint64(b[0:], h.SPIi)
	binary.BigEndian.PutUint64(b[8:], h.SPIr)
	b[16] = h.NextPayload
	b[17] = 0x20
	b[18] = h.Exchange
	b[19] = h.Flags
	binary.BigEndian.PutUint32(b[20:], h.MessageID)
	binary.BigEndian.PutUint32(b[24:], h.Length)
}

func parseHeader(b []byte) (Header, error) {
	if len(b) < headerLen {
		return Header{}, ErrShort
	}
	h := Header{
		SPIi:        binary.BigEndian.Uint64(b[0:]),
		SPIr:        binary.BigEndian.Uint64(b[8:]),
		NextPayload: b[16],
		Exchange:    b[18],
		Flags:       b[19],
		MessageID:   binary.BigEndian.Uint32(b[20:]),
		Length:      binary.BigEndian.Uint32(b[24:]),
	}
	if b[17]>>4 != 2 {
		return h, fmt.Errorf("ike: unsupported major version %d", b[17]>>4)
	}
	if int(h.Length) > len(b) || h.Length < headerLen {
		return h, fmt.Errorf("ike: bad length %d (have %d)", h.Length, len(b))
	}
	return h, nil
}

// Payload is one generic payload. Body excludes the 4-byte generic header.
type Payload struct {
	Type       uint8
	Critical   bool
	Body       []byte
	innerFirst uint8 // SK/SKF only: type of the first encrypted payload
}

// encodePayloads serialises a chain and returns the type of the first one.
func encodePayloads(ps []Payload) (uint8, []byte) {
	if len(ps) == 0 {
		return PayloadNone, nil
	}
	size := 0
	for _, p := range ps {
		size += 4 + len(p.Body)
	}
	out := make([]byte, 0, size)
	for i, p := range ps {
		next := uint8(PayloadNone)
		if i+1 < len(ps) {
			next = ps[i+1].Type
		}
		var crit byte
		if p.Critical {
			crit = 0x80
		}
		out = append(out, next, crit, 0, 0)
		binary.BigEndian.PutUint16(out[len(out)-2:], uint16(4+len(p.Body)))
		out = append(out, p.Body...)
	}
	return ps[0].Type, out
}

// decodePayloads parses a payload chain starting with type first.
func decodePayloads(first uint8, b []byte) ([]Payload, error) {
	var ps []Payload
	next := first
	for next != PayloadNone {
		if len(b) < 4 {
			return nil, fmt.Errorf("ike: truncated payload %d", next)
		}
		l := int(binary.BigEndian.Uint16(b[2:]))
		if l < 4 || l > len(b) {
			return nil, fmt.Errorf("ike: bad payload length %d for type %d", l, next)
		}
		p := Payload{Type: next, Critical: b[1]&0x80 != 0, Body: b[4:l]}
		ps = append(ps, p)
		next = b[0]
		b = b[l:]
		if p.Type == PayloadSK || p.Type == PayloadSKF {
			// Encrypted payload is always last; its "next" names the
			// first inner payload.
			ps[len(ps)-1].innerFirst = next
			break
		}
	}
	return ps, nil
}

// Message is a decoded (and, if applicable, decrypted) IKE message.
type Message struct {
	Header   Header
	Payloads []Payload
	raw      []byte // wire bytes (needed for AUTH signed octets)
}

func (m *Message) find(t uint8) *Payload {
	for i := range m.Payloads {
		if m.Payloads[i].Type == t {
			return &m.Payloads[i]
		}
	}
	return nil
}

func (m *Message) all(t uint8) []Payload {
	var out []Payload
	for _, p := range m.Payloads {
		if p.Type == t {
			out = append(out, p)
		}
	}
	return out
}

// Notify payload.
type Notify struct {
	Protocol uint8
	SPI      []byte
	Type     uint16
	Data     []byte
}

func (n Notify) payload() Payload {
	b := make([]byte, 4+len(n.SPI)+len(n.Data))
	b[0] = n.Protocol
	b[1] = byte(len(n.SPI))
	binary.BigEndian.PutUint16(b[2:], n.Type)
	copy(b[4:], n.SPI)
	copy(b[4+len(n.SPI):], n.Data)
	return Payload{Type: PayloadNotify, Body: b}
}

func parseNotify(b []byte) (Notify, error) {
	if len(b) < 4 || len(b) < 4+int(b[1]) {
		return Notify{}, errors.New("ike: short notify")
	}
	spi := int(b[1])
	return Notify{Protocol: b[0], SPI: b[4 : 4+spi], Type: binary.BigEndian.Uint16(b[2:]), Data: b[4+spi:]}, nil
}

func (m *Message) notifies() []Notify {
	var out []Notify
	for _, p := range m.all(PayloadNotify) {
		if n, err := parseNotify(p.Body); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (m *Message) notify(t uint16) (Notify, bool) {
	for _, n := range m.notifies() {
		if n.Type == t {
			return n, true
		}
	}
	return Notify{}, false
}

// errorNotify returns the first error-class notify (type < 16384).
func (m *Message) errorNotify() (Notify, bool) {
	for _, n := range m.notifies() {
		if n.Type < 16384 {
			return n, true
		}
	}
	return Notify{}, false
}

// NotifyError reports an error notify received from the peer.
type NotifyError struct {
	Type uint16
	Data []byte
}

func (e *NotifyError) Error() string {
	name := map[uint16]string{
		NotifyNoProposalChosen: "NO_PROPOSAL_CHOSEN", NotifyInvalidKEPayload: "INVALID_KE_PAYLOAD",
		NotifyAuthenticationFailed: "AUTHENTICATION_FAILED", NotifyInvalidSyntax: "INVALID_SYNTAX",
		NotifyInternalAddressFailure: "INTERNAL_ADDRESS_FAILURE", NotifyFailedCPRequired: "FAILED_CP_REQUIRED",
		NotifyTSUnacceptable: "TS_UNACCEPTABLE", NotifyTemporaryFailure: "TEMPORARY_FAILURE",
		NotifyNoAdditionalSAs: "NO_ADDITIONAL_SAS", NotifySinglePairRequired: "SINGLE_PAIR_REQUIRED",
		// TS 24.302 table 8.1.2.2-1 (private error notify types).
		8192: "PDN_CONNECTION_REJECTION", 8193: "MAX_CONNECTION_REACHED",
		9000: "NON_3GPP_ACCESS_TO_EPC_NOT_ALLOWED", 9001: "USER_UNKNOWN", 9002: "NO_APN_SUBSCRIPTION",
		9003: "AUTHORIZATION_REJECTED", 9006: "ILLEGAL_ME", 10500: "NETWORK_FAILURE",
		11001: "RAT_TYPE_NOT_ALLOWED", 11005: "IMEI_NOT_ACCEPTED", 11011: "PLMN_NOT_ALLOWED",
		11055: "UNAUTHENTICATED_EMERGENCY_NOT_SUPPORTED",
	}[e.Type]
	if name == "" {
		name = fmt.Sprintf("notify %d", e.Type)
	}
	return "ike: peer reported " + name
}

// Traffic selectors.
type TrafficSelector struct {
	Type      uint8 // 7 = IPv4 range, 8 = IPv6 range
	Protocol  uint8
	StartPort uint16
	EndPort   uint16
	Start     []byte
	End       []byte
}

func encodeTS(ts []TrafficSelector) []byte {
	b := []byte{byte(len(ts)), 0, 0, 0}
	for _, t := range ts {
		l := 8 + len(t.Start) + len(t.End)
		e := make([]byte, 8, l)
		e[0] = t.Type
		e[1] = t.Protocol
		binary.BigEndian.PutUint16(e[2:], uint16(l))
		binary.BigEndian.PutUint16(e[4:], t.StartPort)
		binary.BigEndian.PutUint16(e[6:], t.EndPort)
		e = append(e, t.Start...)
		e = append(e, t.End...)
		b = append(b, e...)
	}
	return b
}

func decodeTS(b []byte) ([]TrafficSelector, error) {
	if len(b) < 4 {
		return nil, errors.New("ike: short TS payload")
	}
	n := int(b[0])
	b = b[4:]
	var out []TrafficSelector
	for i := 0; i < n; i++ {
		if len(b) < 8 {
			return nil, errors.New("ike: short traffic selector")
		}
		l := int(binary.BigEndian.Uint16(b[2:]))
		if l < 8 || l > len(b) {
			return nil, errors.New("ike: bad traffic selector length")
		}
		addrLen := (l - 8) / 2
		out = append(out, TrafficSelector{
			Type: b[0], Protocol: b[1],
			StartPort: binary.BigEndian.Uint16(b[4:]), EndPort: binary.BigEndian.Uint16(b[6:]),
			Start: b[8 : 8+addrLen], End: b[8+addrLen : l],
		})
		b = b[l:]
	}
	return out, nil
}

func allTrafficSelectors() []TrafficSelector {
	v6s, v6e := make([]byte, 16), make([]byte, 16)
	for i := range v6e {
		v6e[i] = 0xff
	}
	return []TrafficSelector{
		{Type: 7, StartPort: 0, EndPort: 65535, Start: []byte{0, 0, 0, 0}, End: []byte{255, 255, 255, 255}},
		{Type: 8, StartPort: 0, EndPort: 65535, Start: v6s, End: v6e},
	}
}
