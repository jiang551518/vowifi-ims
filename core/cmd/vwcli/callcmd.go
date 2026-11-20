package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"vowifi/ims"
	"vowifi/netio"
	"vowifi/sim"
)

// call <subId> -dial NUMBER | -answer: signalling test with an audio echo
// (received AMR frames are sent straight back, so the far end hears itself).
func cmdCall(ctx context.Context, h *sim.Helper, args []string) error {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	var o tunnelOpts
	o.register(fs)
	dial := fs.String("dial", "", "number to call")
	answer := fs.Bool("answer", false, "auto-answer incoming calls after 3s")
	dur := fs.Duration("dur", 30*time.Second, "hang up after this long once active")
	hold := fs.Duration("hold", 180*time.Second, "overall time limit")
	scheme := fs.String("scheme", "sip", "dial URI scheme: sip|tel")
	instance := fs.String("instance", "", "+sip.instance UUID (default: IMEI-based)")
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
	country := map[string]string{"234": "44", "460": "86", "454": "852"}[t.sub.MCC]
	events := make(chan ims.CallInfo, 32)
	var ua *ims.UA
	ua, err = ims.Start(ctx, ims.Config{
		IMSI: t.id.IMSI, MCC: t.sub.MCC, MNC: t.sub.MNC, IMPI: t.id.IMPI, Domain: t.id.Domain,
		IMPU: t.id.IMPU, IMEI: t.id.IMEI, InstanceID: *instance, AKA: t.aka, PCSCF: info.PCSCF, Stack: st, SecAgree: true,
		DialURIScheme: *scheme, UserEqPhone: true, SMSCAddress: t.id.SMSC, CountryCode: country,
		OnEvent: func(e ims.Event) {
			b, _ := json.Marshal(e)
			fmt.Println("EVENT", string(b))
			if c, ok := e.Data.(ims.CallInfo); ok {
				events <- c
			}
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
	var rx, tx int
	ua.SetMediaHandlers(func(id string, f ims.AudioFrame) {
		rx++
		if ua.SendAudio(id, f.Frame) == nil {
			tx++
		}
	}, func(id string, d rune) { fmt.Println("DTMF", string(d)) })
	fmt.Println("IMS REGISTERED, number", ua.Registration().Number != "")
	if *dial != "" {
		if _, err := ua.Dial(ctx, *dial); err != nil {
			return err
		}
	}
	deadline := time.After(*hold)
	for {
		select {
		case c := <-events:
			switch c.State {
			case ims.CallIncoming:
				if *answer {
					go func(id string) {
						time.Sleep(3 * time.Second)
						fmt.Println("answer:", ua.Answer(id))
					}(c.ID)
				}
			case ims.CallActive:
				go func(id string) {
					time.Sleep(*dur)
					fmt.Printf("hanging up; rtp frames rx=%d echoed=%d\n", rx, tx)
					_ = ua.Hangup(context.Background(), id)
				}(c.ID)
			case ims.CallEnded:
				fmt.Printf("call ended: %s (%d) rx=%d echoed=%d\n", c.Reason, c.SIPCode, rx, tx)
				if *dial != "" {
					return nil
				}
			}
		case <-deadline:
			return nil
		case <-t.sess.Done():
			return t.sess.Err()
		}
	}
}
