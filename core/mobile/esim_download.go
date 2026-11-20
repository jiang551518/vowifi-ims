package mobile

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	euicchttp "github.com/damonto/euicc-go/http"
	"github.com/damonto/euicc-go/http/rootci"
	"github.com/damonto/euicc-go/lpa"
	sgp22 "github.com/damonto/euicc-go/v2"
)

// DownloadListener receives progress during an eSIM profile download. All
// callbacks run on the download goroutine; Confirm may block until the user
// answers.
type DownloadListener interface {
	// Progress reports a stage: "auth-client" | "auth-server" | "install" | "notify".
	Progress(stage string)
	// Confirm shows the profile the SM-DP+ is about to send (JSON with name,
	// provider, iccid, class) and returns true to continue.
	Confirm(profileJSON string) bool
	// ConfirmationCode is asked when the SM-DP+ requires one and none was given.
	ConfirmationCode() string
}

// omapiChannel adapts an OMAPI channel already opened on the ISD-R to
// euicc-go's SmartCardChannel. The Android secure element service selects the
// applet and puts the logical channel number into CLA itself, so the
// logical channel reported here is only a placeholder.
type omapiChannel struct{ ch APDUChannel }

func (o omapiChannel) Connect() error                          { return nil }
func (o omapiChannel) Disconnect() error                       { return nil }
func (o omapiChannel) OpenLogicalChannel([]byte) (byte, error) { return 1, nil }
func (o omapiChannel) CloseLogicalChannel(byte) error          { return nil }
func (o omapiChannel) Transmit(command []byte) ([]byte, error) {
	return o.ch.Transmit(command)
}

// EsimDownloadChannel downloads and installs a profile (SGP.22 ES9+/ES10b)
// onto the eUICC behind channel. activationCode is "LPA:1$<SM-DP+>$<matching ID>".
// It returns JSON {"iccid","name","provider","notified"}.
func EsimDownloadChannel(channel APDUChannel, activationCode, confirmationCode, imei string, resolver Resolver, l DownloadListener) (string, error) {
	if channel == nil {
		return "", errors.New("esim: no channel")
	}
	var ac lpa.ActivationCode
	if err := ac.UnmarshalText([]byte(strings.TrimSpace(activationCode))); err != nil {
		return "", fmt.Errorf("激活码格式不正确：%w", err)
	}
	ac.ConfirmationCode = strings.TrimSpace(confirmationCode)
	ac.IMEI = imei
	if ac.IMEI == "" {
		ac.IMEI = "350000000000000"
	}

	client, err := lpa.New(&lpa.Options{
		Channel: omapiChannel{channel},
		Logger:  slog.New(slog.NewTextHandler(discard{}, nil)),
		Timeout: 60 * time.Second,
	})
	if err != nil {
		return "", err
	}
	defer client.Close()
	hc, err := rspHTTPClient(resolver, 60*time.Second)
	if err != nil {
		return "", err
	}
	client.HTTP = &euicchttp.Client{Client: hc, AdminProtocolVersion: client.HTTP.AdminProtocolVersion}

	var meta *sgp22.ProfileInfo
	stages := map[lpa.DownloadStage]string{
		lpa.DownloadStageAuthenticateClient: "auth-client",
		lpa.DownloadStageAuthenticateServer: "auth-server",
		lpa.DownloadStageInstall:            "install",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result, err := client.DownloadProfile(ctx, &ac, &lpa.DownloadOptions{
		OnProgress: func(s lpa.DownloadStage) {
			if l != nil {
				l.Progress(stages[s])
			}
		},
		OnConfirm: func(m *sgp22.ProfileInfo) bool {
			meta = m
			if l == nil {
				return true
			}
			return l.Confirm(profileJSON(m))
		},
		OnEnterConfirmationCode: func() string {
			if l == nil {
				return ""
			}
			return strings.TrimSpace(l.ConfirmationCode())
		},
	})
	if err != nil {
		return "", err
	}
	if result == nil {
		// The user declined (or the context ended); the session was cancelled.
		return "", errors.New("已取消下载")
	}

	out := map[string]any{}
	if meta != nil {
		_ = json.Unmarshal([]byte(profileJSON(meta)), &out)
	}
	// Report the install to the SM-DP+ (SGP.22 §3.5) and drop it from the card's list.
	notified := false
	if result.Notification != nil {
		if l != nil {
			l.Progress("notify")
		}
		if list, err := client.RetrieveNotificationList(result.Notification.SequenceNumber); err == nil {
			for _, n := range list {
				if client.HandleNotification(n) == nil {
					notified = client.RemoveNotificationFromList(n.Notification.SequenceNumber) == nil
				}
			}
		}
	}
	out["notified"] = notified
	data, err := json.Marshal(out)
	return string(data), err
}

func profileJSON(m *sgp22.ProfileInfo) string {
	if m == nil {
		return "{}"
	}
	data, _ := json.Marshal(map[string]any{
		"iccid":    m.ICCID.String(),
		"name":     m.ProfileName,
		"provider": m.ServiceProviderName,
		"class":    m.ProfileClass.String(),
	})
	return string(data)
}

// rspHTTPClient talks to SM-DP+ servers. Hosts are resolved through Android
// (the Go resolver has no resolv.conf there), and a server certificate is
// accepted if it chains to a GSMA eUICC CI root or to a system web root.
func rspHTTPClient(resolver Resolver, timeout time.Duration) (*http.Client, error) {
	gsma, err := rootci.TrustedRootCAs()
	if err != nil {
		return nil, err
	}
	system, _ := x509.SystemCertPool()
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil || resolver == nil || net.ParseIP(host) != nil {
				return dialer.DialContext(ctx, network, addr)
			}
			s, err := resolver.Resolve(host, 0)
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", host, err)
			}
			var last error
			for _, p := range strings.Split(s, ",") {
				ip := strings.TrimSpace(p)
				if net.ParseIP(ip) == nil {
					continue
				}
				c, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			if last == nil {
				last = fmt.Errorf("resolve %s: no address", host)
			}
			return nil, last
		},
		TLSHandshakeTimeout: 20 * time.Second,
		TLSClientConfig: &tls.Config{
			// Verification is done below against two root pools.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				return verifyChain(cs, gsma, system)
			},
		},
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

func verifyChain(cs tls.ConnectionState, pools ...*x509.CertPool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("tls: no server certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	var last error
	for _, roots := range pools {
		if roots == nil {
			continue
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
			DNSName:       cs.ServerName,
			Roots:         roots,
			Intermediates: inter,
		})
		if err == nil {
			return nil
		}
		last = err
	}
	return fmt.Errorf("SM-DP+ 证书校验失败：%w", last)
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
