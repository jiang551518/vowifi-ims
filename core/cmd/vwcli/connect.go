package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"vowifi/ike"
	"vowifi/sim"
)

type tunnelOpts struct {
	dns, static, epdg, socks string
	app                      int
	devid                    bool
	nai                      string
}

func (o *tunnelOpts) register(fs *flag.FlagSet) {
	fs.StringVar(&o.dns, "dns", "system", "system|doh|static")
	fs.StringVar(&o.static, "ip", "", "static ePDG IP (with -dns static)")
	fs.StringVar(&o.epdg, "epdg", "", "override ePDG FQDN")
	fs.StringVar(&o.socks, "socks", "", "SOCKS5 proxy host:port")
	fs.IntVar(&o.app, "app", sim.AppUSIM, "UICC app for AKA (2=USIM,5=ISIM)")
	fs.BoolVar(&o.devid, "devid", true, "send DEVICE_IDENTITY")
	fs.StringVar(&o.nai, "nai", "", "identity template, {imsi} is replaced (default: TS 23.003 root NAI)")
}

type tunnelUp struct {
	sess *ike.Session
	sub  sim.Subscription
	id   sim.Identity
	aka  ike.AKAFunc
}

func bringUp(ctx context.Context, h *sim.Helper, subID int, o tunnelOpts) (*tunnelUp, error) {
	subs, err := h.Subscriptions(ctx)
	if err != nil {
		return nil, err
	}
	var sub *sim.Subscription
	for i := range subs {
		if subs[i].SubID == subID {
			sub = &subs[i]
		}
	}
	if sub == nil {
		return nil, fmt.Errorf("subId %d not active", subID)
	}
	id, err := h.Identity(ctx, subID, sub.Slot)
	if err != nil {
		return nil, err
	}
	host := o.epdg
	if host == "" {
		host = ike.EPDGHost(sub.MCC, sub.MNC)
	}
	nc := ike.NetConfig{DNSMode: o.dns, SOCKS5: o.socks}
	if o.static != "" {
		nc.StaticIPs = strings.Split(o.static, ",")
	}
	ips, err := nc.Resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	slog.Info("ePDG resolved", "host", host, "ips", ips)
	tr, err := nc.Dial(ctx, ips[0])
	if err != nil {
		return nil, err
	}
	app := o.app
	aka := func(ctx context.Context, rand, autn []byte) (res, ck, ik, auts []byte, err error) {
		r, err := h.AKA(ctx, subID, app, rand, autn)
		if errors.Is(err, sim.ErrSyncFailure) {
			slog.Warn("AKA sync failure, sending AUTS")
			return nil, nil, nil, r.AUTS, nil
		}
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return r.RES, r.CK, r.IK, nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	start := time.Now()
	s, err := ike.Connect(cctx, tr, ike.Config{
		Identity:       naiFor(o.nai, id.IMSI, sub.MCC, sub.MNC),
		AKA:            aka,
		IMEI:           id.IMEI,
		DeviceIdentity: o.devid,
	})
	if err != nil {
		return nil, err
	}
	info := s.Info()
	fmt.Printf("TUNNEL UP in %v\n  ike=%s esp=%s auth=%s\n  ipv4=%v ipv6=%v/%d\n  pcscf=%v\n",
		time.Since(start).Round(time.Millisecond), info.IKESuite, info.ESPSuite, info.ResponderAuth,
		info.IPv4, info.IPv6, info.IPv6Prefix, info.PCSCF)
	return &tunnelUp{sess: s, sub: *sub, id: id, aka: aka}, nil
}

// connect <subId> [flags]: bring up the SWu tunnel and hold it.
func cmdConnect(ctx context.Context, h *sim.Helper, args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	var o tunnelOpts
	o.register(fs)
	hold := fs.Duration("hold", 20*time.Second, "keep tunnel up for")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	var subID int
	fmt.Sscanf(args[0], "%d", &subID)
	t, err := bringUp(ctx, h, subID, o)
	if err != nil {
		return err
	}
	select {
	case <-time.After(*hold):
	case <-t.sess.Done():
		fmt.Println("tunnel ended:", t.sess.Err())
	}
	return t.sess.Close()
}

func naiFor(tmpl, imsi, mcc, mnc string) string {
	if tmpl == "" {
		return ike.NAI(imsi, mcc, mnc)
	}
	return strings.ReplaceAll(tmpl, "{imsi}", imsi)
}
