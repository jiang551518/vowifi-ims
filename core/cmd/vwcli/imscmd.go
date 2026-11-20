package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"vowifi/ims"
	"vowifi/netio"
	"vowifi/sim"
)

// ims <subId> [flags]: tunnel + IMS registration, optional SMS, then wait.
func cmdIMS(ctx context.Context, h *sim.Helper, args []string) error {
	fs := flag.NewFlagSet("ims", flag.ContinueOnError)
	var o tunnelOpts
	o.register(fs)
	hold := fs.Duration("hold", 60*time.Second, "stay registered for")
	sms := fs.String("sms", "", "send SMS: number:text")
	tcp := fs.Bool("tcp", false, "SIP over TCP")
	secagree := fs.Bool("secagree", true, "use IPsec sec-agree")
	scheme := fs.String("scheme", "sip", "dial/SMSC URI scheme: sip|tel")
	cc := fs.String("cc", "", "country code for local numbers (default from SIM)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	var subID int
	fmt.Sscanf(args[0], "%d", &subID)
	t, err := bringUp(ctx, h, subID, o)
	if err != nil {
		return err
	}
	defer t.sess.Close()
	info := t.sess.Info()
	st, err := netio.New(t.sess, info.IPv4, info.IPv6, slog.Default())
	if err != nil {
		return err
	}
	defer st.Close()
	country := *cc
	if country == "" {
		country = map[string]string{"234": "44", "460": "86", "454": "852", "310": "1", "311": "1"}[t.sub.MCC]
	}
	ua, err := ims.Start(ctx, ims.Config{
		IMSI: t.id.IMSI, MCC: t.sub.MCC, MNC: t.sub.MNC,
		IMPI: t.id.IMPI, Domain: t.id.Domain, IMPU: t.id.IMPU, IMEI: t.id.IMEI,
		AKA: t.aka, PCSCF: info.PCSCF, Stack: st,
		SecAgree: *secagree, PreferTCP: *tcp,
		DialURIScheme: *scheme, UserEqPhone: true,
		SMSCAddress: t.id.SMSC, CountryCode: country,
		OnEvent: func(e ims.Event) {
			b, _ := json.Marshal(e)
			fmt.Println("EVENT", string(b))
		},
	})
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ua.Close(c)
		cancel()
	}()
	reg := ua.Registration()
	fmt.Printf("IMS REGISTERED number=%s security=%s expires=%d\n", reg.Number, reg.Security, reg.Expires)
	if *sms != "" {
		to, text, _ := strings.Cut(*sms, ":")
		sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		res, err := ua.SendSMS(sctx, to, text, true)
		cancel()
		fmt.Printf("SMS result: %+v err=%v\n", res, err)
	}
	select {
	case <-time.After(*hold):
	case <-t.sess.Done():
		fmt.Println("tunnel ended:", t.sess.Err())
	}
	return nil
}
