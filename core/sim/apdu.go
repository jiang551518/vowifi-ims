package sim

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ChannelTarget selects the UICC for logical channel access. Use SubID for a
// card with an active subscription; use Slot (physical slot, SubID = -1) for
// a removable eUICC whose profiles are all disabled.
type ChannelTarget struct {
	SubID int
	Slot  int
	Port  int
}

func (t ChannelTarget) params() map[string]any {
	return map[string]any{"subId": t.SubID, "slot": t.Slot, "port": t.Port}
}

// Channel is an open logical channel to an applet.
type Channel struct {
	h      *Helper
	target ChannelTarget
	ID     int
	Select []byte
}

// OpenChannel opens a logical channel and selects the applet aid (hex).
func (h *Helper) OpenChannel(ctx context.Context, target ChannelTarget, aid string) (*Channel, error) {
	p := target.params()
	p["aid"] = strings.ToUpper(aid)
	var resp struct {
		Channel int    `json:"channel"`
		Select  string `json:"select"`
	}
	if err := h.Call(ctx, "open", p, &resp); err != nil {
		return nil, err
	}
	sel, _ := hex.DecodeString(resp.Select)
	return &Channel{h: h, target: target, ID: resp.Channel, Select: sel}, nil
}

// Close closes the logical channel.
func (c *Channel) Close(ctx context.Context) error {
	p := c.target.params()
	p["channel"] = c.ID
	return c.h.Call(ctx, "close", p, nil)
}

// APDUError carries a non-success status word.
type APDUError struct{ SW uint16 }

func (e *APDUError) Error() string { return fmt.Sprintf("sim: APDU status %04X", e.SW) }

// Transmit sends one command APDU (CLA INS P1 P2 [Lc data] [Le]) and returns
// the response data with SW1SW2 stripped. 61xx is followed by GET RESPONSE.
func (c *Channel) Transmit(ctx context.Context, apdu []byte) ([]byte, error) {
	data, sw, err := c.transmitRaw(ctx, apdu)
	if err != nil {
		return nil, err
	}
	for sw>>8 == 0x61 {
		var more []byte
		more, sw, err = c.transmitRaw(ctx, []byte{apdu[0] & 0xF3, 0xC0, 0x00, 0x00, byte(sw)})
		if err != nil {
			return nil, err
		}
		data = append(data, more...)
	}
	if sw>>8 == 0x6C { // wrong Le: resend with the right one
		fixed := append([]byte(nil), apdu...)
		fixed[len(fixed)-1] = byte(sw)
		return c.Transmit(ctx, fixed)
	}
	if sw != 0x9000 && sw>>8 != 0x91 {
		return data, &APDUError{SW: sw}
	}
	return data, nil
}

func (c *Channel) transmitRaw(ctx context.Context, apdu []byte) ([]byte, uint16, error) {
	if len(apdu) < 4 {
		return nil, 0, errors.New("sim: APDU too short")
	}
	cla, ins, p1, p2 := apdu[0], apdu[1], apdu[2], apdu[3]
	p3 := -1
	var data []byte
	switch {
	case len(apdu) == 4:
	case len(apdu) == 5:
		p3 = int(apdu[4]) // Le only
	default:
		lc := int(apdu[4])
		if len(apdu) < 5+lc {
			return nil, 0, errors.New("sim: APDU Lc exceeds length")
		}
		p3 = lc
		data = apdu[5 : 5+lc]
	}
	// Encode the logical channel number in CLA (ISO 7816-4 §5.4.1).
	if c.ID > 0 && c.ID < 4 {
		cla = (cla & 0xFC) | byte(c.ID)
	} else if c.ID >= 4 {
		cla = (cla & 0xB0) | 0x40 | byte(c.ID-4)
	}
	p := c.target.params()
	p["channel"] = c.ID
	p["cla"] = int(cla)
	p["ins"] = int(ins)
	p["p1"] = int(p1)
	p["p2"] = int(p2)
	p["p3"] = p3
	p["data"] = strings.ToUpper(hex.EncodeToString(data))
	var resp struct {
		Response string `json:"response"`
	}
	if err := c.h.Call(ctx, "transmit", p, &resp); err != nil {
		return nil, 0, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(resp.Response))
	if err != nil || len(raw) < 2 {
		return nil, 0, fmt.Errorf("sim: bad APDU response %q", resp.Response)
	}
	sw := uint16(raw[len(raw)-2])<<8 | uint16(raw[len(raw)-1])
	return raw[:len(raw)-2], sw, nil
}
