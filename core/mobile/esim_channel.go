package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vowifi/esim"
)

// APDUChannel is an already authorised channel supplied by Android OMAPI.
// Transmit returns the complete response including SW1/SW2. Android owns its lifetime.
type APDUChannel interface {
	Transmit(command []byte) ([]byte, error)
}

// ReadEsimChannelJSON reads metadata and profiles, without requiring a telephony helper.
func ReadEsimChannelJSON(channel APDUChannel, aidName string) (string, error) {
	if channel == nil {
		return "", errors.New("esim: no channel")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	card := esim.NewReader(func(ctx context.Context, cmd []byte) ([]byte, error) { return exchangeAPDU(ctx, channel, cmd) }, aidName)
	info, err := card.ReadInfo(ctx)
	if err != nil {
		return "", err
	}
	profiles, err := card.Profiles(ctx)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(map[string]any{"info": info, "profiles": profiles, "source": "omapi", "readOnly": false})
	return string(data), err
}

// EsimProfileOpChannel runs enable | disable | delete | nickname on a profile
// over an OMAPI channel to the ISD-R that the card's access rules granted.
// Enabling/disabling makes the card REFRESH, so the modem re-reads it.
func EsimProfileOpChannel(channel APDUChannel, aidName, op, iccid, arg string) error {
	if channel == nil {
		return errors.New("esim: no channel")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	card := esim.NewReader(func(ctx context.Context, cmd []byte) ([]byte, error) { return exchangeAPDU(ctx, channel, cmd) }, aidName)
	switch op {
	case "enable":
		return card.Enable(ctx, iccid)
	case "disable":
		return card.Disable(ctx, iccid)
	case "delete":
		return card.Delete(ctx, iccid)
	case "nickname":
		return card.SetNickname(ctx, iccid, arg)
	}
	return fmt.Errorf("esim: unknown operation %q", op)
}

func exchangeAPDU(ctx context.Context, channel APDUChannel, command []byte) ([]byte, error) {
	if len(command) < 4 {
		return nil, errors.New("esim: short APDU")
	}
	next := append([]byte(nil), command...)
	var out []byte
	corrected := false
	for round := 0; round < 64; round++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := channel.Transmit(next)
		if err != nil {
			return nil, err
		}
		if len(raw) < 2 {
			return nil, errors.New("esim: incomplete APDU response")
		}
		sw1, sw2 := raw[len(raw)-2], raw[len(raw)-1]
		body := raw[:len(raw)-2]
		if sw1 == 0x6c && !corrected && len(next) > 4 {
			next[len(next)-1] = sw2
			corrected = true
			continue
		}
		out = append(out, body...)
		if len(out) > 1024*1024 {
			return nil, errors.New("esim: response too large")
		}
		if sw1 == 0x61 {
			next = []byte{command[0], 0xc0, 0, 0, sw2}
			corrected = false
			continue
		}
		if sw1 == 0x90 && sw2 == 0 || sw1 == 0x91 {
			return out, nil
		}
		return nil, fmt.Errorf("esim: APDU status %02X%02X", sw1, sw2)
	}
	return nil, errors.New("esim: APDU response chain limit reached")
}
