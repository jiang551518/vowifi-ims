// Package ims implements an IMS user agent (TS 24.229): registration with
// IMS-AKA and sec-agree, SMS over IP (TS 24.341) and MMTel voice calls.
package ims

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"
)

// Message is a SIP request or response. Header names are canonicalised to
// their long form; values keep their order.
type Message struct {
	Method     string // requests only
	RequestURI string
	StatusCode int // responses only
	Reason     string
	headers    []header
	Body       []byte
	origin     *origin // incoming requests: where to send responses
}

type header struct {
	name  string
	value string
}

var compact = map[string]string{
	"i": "Call-ID", "m": "Contact", "e": "Content-Encoding", "l": "Content-Length",
	"c": "Content-Type", "f": "From", "s": "Subject", "k": "Supported", "t": "To",
	"v": "Via", "r": "Refer-To", "b": "Referred-By", "o": "Event", "u": "Allow-Events",
	"a": "Accept-Contact", "j": "Reject-Contact", "d": "Request-Disposition", "x": "Session-Expires",
	"y": "Identity",
}

var special = map[string]string{
	"call-id": "Call-ID", "cseq": "CSeq", "www-authenticate": "WWW-Authenticate",
	"p-associated-uri": "P-Associated-URI", "p-asserted-identity": "P-Asserted-Identity",
	"p-preferred-identity": "P-Preferred-Identity", "p-access-network-info": "P-Access-Network-Info",
	"p-preferred-service": "P-Preferred-Service", "p-asserted-service": "P-Asserted-Service",
	"p-charging-vector": "P-Charging-Vector", "p-called-party-id": "P-Called-Party-ID",
	"rseq": "RSeq", "rack": "RAck", "p-early-media": "P-Early-Media", "sip-etag": "SIP-ETag",
}

func canonical(name string) string {
	name = strings.TrimSpace(name)
	if c, ok := compact[strings.ToLower(name)]; ok {
		return c
	}
	if s, ok := special[strings.ToLower(name)]; ok {
		return s
	}
	return textproto.CanonicalMIMEHeaderKey(name)
}

// IsRequest reports whether m is a request.
func (m *Message) IsRequest() bool { return m.Method != "" }

// Get returns the first value of a header.
func (m *Message) Get(name string) string {
	name = canonical(name)
	for _, h := range m.headers {
		if h.name == name {
			return h.value
		}
	}
	return ""
}

// Values returns all values of a header, splitting comma lists for headers
// that allow it.
func (m *Message) Values(name string) []string {
	name = canonical(name)
	var out []string
	for _, h := range m.headers {
		if h.name != name {
			continue
		}
		if splittable(name) {
			out = append(out, splitComma(h.value)...)
		} else {
			out = append(out, h.value)
		}
	}
	return out
}

func splittable(name string) bool {
	switch name {
	case "WWW-Authenticate", "Authorization", "Proxy-Authenticate", "Date", "User-Agent", "Server",
		"Subject", "P-Access-Network-Info", "P-Charging-Vector":
		return false
	}
	return true
}

// splitComma splits on commas that are outside quotes and angle brackets.
func splitComma(s string) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case '<':
			if !quoted {
				depth++
			}
		case '>':
			if !quoted && depth > 0 {
				depth--
			}
		case ',':
			if !quoted && depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, t)
	}
	return out
}

// Add appends a header.
func (m *Message) Add(name, value string) {
	m.headers = append(m.headers, header{canonical(name), value})
}

// Set replaces all values of a header.
func (m *Message) Set(name, value string) {
	m.Del(name)
	m.Add(name, value)
}

// Del removes a header.
func (m *Message) Del(name string) {
	name = canonical(name)
	out := m.headers[:0]
	for _, h := range m.headers {
		if h.name != name {
			out = append(out, h)
		}
	}
	m.headers = out
}

// Bytes serialises the message, setting Content-Length.
func (m *Message) Bytes() []byte {
	var b bytes.Buffer
	if m.IsRequest() {
		fmt.Fprintf(&b, "%s %s SIP/2.0\r\n", m.Method, m.RequestURI)
	} else {
		fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", m.StatusCode, m.Reason)
	}
	for _, h := range m.headers {
		if h.name == "Content-Length" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\r\n", h.name, h.value)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(m.Body))
	b.Write(m.Body)
	return b.Bytes()
}

func (m *Message) String() string { return string(m.Bytes()) }

// Parse parses one complete datagram.
func Parse(b []byte) (*Message, error) {
	m, rest, err := readMessage(bufio.NewReader(bytes.NewReader(b)), true)
	_ = rest
	return m, err
}

// ReadStream reads one message from a stream transport (TCP).
func ReadStream(r *bufio.Reader) (*Message, error) {
	m, _, err := readMessage(r, false)
	return m, err
}

func readMessage(r *bufio.Reader, datagram bool) (*Message, []byte, error) {
	tp := textproto.NewReader(r)
	var first string
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(line) != "" { // skip keepalive CRLFs
			first = line
			break
		}
	}
	m := &Message{}
	if strings.HasPrefix(first, "SIP/2.0 ") {
		parts := strings.SplitN(first, " ", 3)
		if len(parts) < 2 {
			return nil, nil, fmt.Errorf("sip: bad status line %q", first)
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, nil, fmt.Errorf("sip: bad status code %q", parts[1])
		}
		m.StatusCode = code
		if len(parts) == 3 {
			m.Reason = parts[2]
		}
	} else {
		parts := strings.Split(first, " ")
		if len(parts) != 3 || parts[2] != "SIP/2.0" {
			return nil, nil, fmt.Errorf("sip: bad request line %q", first)
		}
		m.Method, m.RequestURI = parts[0], parts[1]
	}
	var last *header
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return nil, nil, err
		}
		if line == "" {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && last != nil {
			last.value += " " + strings.TrimSpace(line)
			continue
		}
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		m.headers = append(m.headers, header{canonical(line[:i]), strings.TrimSpace(line[i+1:])})
		last = &m.headers[len(m.headers)-1]
	}
	cl := -1
	if v := m.Get("Content-Length"); v != "" {
		cl, _ = strconv.Atoi(strings.TrimSpace(v))
	}
	switch {
	case cl > 0:
		m.Body = make([]byte, cl)
		if _, err := io.ReadFull(r, m.Body); err != nil {
			return nil, nil, fmt.Errorf("sip: short body: %w", err)
		}
	case cl < 0 && datagram:
		m.Body, _ = io.ReadAll(r)
	case cl < 0:
		return nil, nil, errors.New("sip: stream message without Content-Length")
	}
	return m, nil, nil
}

// ------------------------------------------------------------- helpers

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newBranch() string { return "z9hG4bK" + randHex(10) }
func newTag() string    { return randHex(6) }

// Param returns a ;name=value parameter from a header value (case-insensitive
// name, quotes stripped). Present-without-value returns "", true.
func Param(v, name string) (string, bool) {
	// Ignore parameters inside <...> (URI parameters).
	if i := strings.LastIndexByte(v, '>'); i >= 0 {
		v = v[i+1:]
	}
	for _, p := range strings.Split(v, ";")[1:] {
		k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.Trim(strings.TrimSpace(val), `"`), true
		}
	}
	return "", false
}

// URIOf extracts the URI from a name-addr ("Name" <uri>;params) or addr-spec.
func URIOf(v string) string {
	if i := strings.IndexByte(v, '<'); i >= 0 {
		if j := strings.IndexByte(v[i:], '>'); j >= 0 {
			return v[i+1 : i+j]
		}
	}
	if i := strings.IndexByte(v, ';'); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return strings.TrimSpace(v)
}

// authParams parses a Digest challenge/credentials header.
func authParams(v string) map[string]string {
	out := map[string]string{}
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, ' '); i >= 0 {
		out["scheme"] = v[:i]
		v = v[i+1:]
	}
	for _, p := range splitComma(v) {
		k, val, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(val), `"`)
	}
	return out
}

// NumberFromURI extracts a phone number (with +) from tel: or sip: URIs.
func NumberFromURI(uri string) string {
	u := URIOf(uri)
	switch {
	case strings.HasPrefix(u, "tel:"):
		u = u[4:]
	case strings.HasPrefix(u, "sip:"), strings.HasPrefix(u, "sips:"):
		u = u[strings.IndexByte(u, ':')+1:]
		if i := strings.IndexByte(u, '@'); i >= 0 {
			u = u[:i]
		}
	}
	if i := strings.IndexByte(u, ';'); i >= 0 {
		u = u[:i]
	}
	return u
}
