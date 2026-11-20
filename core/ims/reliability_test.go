package ims

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"vowifi/netio"
)

type memoryLink struct {
	mu      sync.Mutex
	handler func([]byte)
	peer    *memoryLink
}

func (l *memoryLink) SetPacketHandler(h func([]byte)) { l.mu.Lock(); l.handler = h; l.mu.Unlock() }
func (l *memoryLink) SendPacket(b []byte) error {
	l.peer.mu.Lock()
	h := l.peer.handler
	l.peer.mu.Unlock()
	if h != nil {
		h(append([]byte(nil), b...))
	}
	return nil
}

func TestRegisterMinExpires(t *testing.T) {
	for _, tc := range []struct {
		name, minimum string
		wantError     bool
	}{
		{"retry", "600", false}, {"missing", "", true}, {"invalid", "abc", true}, {"not increasing", "120", true}, {"negative", "-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			a, b := &memoryLink{}, &memoryLink{}
			a.peer = b
			b.peer = a
			local, err := netio.New(a, net.ParseIP("10.0.0.1"), nil, log)
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			remote, err := netio.New(b, net.ParseIP("10.0.0.2"), nil, log)
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			server, err := remote.ListenUDP(net.ParseIP("10.0.0.2"), 5060)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			tr, err := NewTransport(local, net.ParseIP("10.0.0.2"), 31000, 31001, log)
			if err != nil {
				t.Fatal(err)
			}
			defer tr.Close()
			ua := &UA{cfg: Config{Domain: "example.test", Expires: 120}, tr: tr, log: log, impu: "sip:test@example.test", callID: "test", fromTag: "tag"}
			requests := make(chan *Message, 2)
			go func() {
				buf := make([]byte, 65536)
				for i := 0; i < 2; i++ {
					n, addr, e := server.ReadFrom(buf)
					if e != nil {
						return
					}
					req, e := Parse(buf[:n])
					if e != nil {
						return
					}
					requests <- req
					code, reason := 423, "Interval Too Brief"
					if i == 1 {
						code, reason = 200, "OK"
					}
					resp := NewResponse(req, code, reason)
					resp.Add("Min-Expires", tc.minimum)
					resp.Add("Expires", "600")
					if _, e = server.WriteTo(resp.Bytes(), addr); e != nil {
						return
					}
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err = ua.register(ctx, true)
			if (err != nil) != tc.wantError {
				t.Fatalf("register error = %v", err)
			}
			if !tc.wantError {
				first, second := <-requests, <-requests
				if second.Get("Expires") != "600" || first.Get("CSeq") == second.Get("CSeq") || !ua.Registration().Registered {
					t.Fatal("retry must increase CSeq and Expires and register")
				}
			}
		})
	}
}

func TestTCPDisconnectSignalsFailure(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	tc := &tcpConn{c: a}
	tr := &Transport{clientTCP: tc, failures: make(chan error, 1)}
	go tr.readTCP(tc)
	b.Close()
	select {
	case <-tr.failures:
	case <-time.After(time.Second):
		t.Fatal("disconnect was not reported")
	}
}

func TestRegistrationFailureWakesSupervisor(t *testing.T) {
	ua := &UA{log: slog.New(slog.NewTextHandler(io.Discard, nil)), tr: &Transport{failures: make(chan error, 1)}, stop: make(chan struct{}), done: make(chan struct{}), expires: 600, registered: true}
	go ua.refreshLoop()
	ua.tr.failures <- errors.New("proxy unavailable")
	select {
	case <-ua.Done():
	case <-time.After(time.Second):
		t.Fatal("supervisor was not notified")
	}
	ua.mu.Lock()
	registered := ua.registered
	ua.mu.Unlock()
	if registered {
		t.Fatal("still registered after failure")
	}
}
