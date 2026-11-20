package esim

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"vowifi/sim"
)

// Known ISD-R AIDs: the GSMA standard one and vendor variants used by
// removable eUICC products.
var ISDRAIDs = []struct{ Name, AID string }{
	{"GSMA", "A0000005591010FFFFFFFF8900000100"},
	{"5ber", "A0000005591010FFFFFFFF8900050500"},
	{"esim.me", "A0000005591010000000008900000300"},
	{"XeSIM", "A0000005591010FFFFFFFF8900000177"},
}

// Card is an open ES10 session with an eUICC.
type Card struct {
	ch       *sim.Channel
	transmit func(context.Context, []byte) ([]byte, error)
	AIDName  string
	AID      string
}

// Open selects the ISD-R on the target UICC, trying known AIDs.
func Open(ctx context.Context, h *sim.Helper, target sim.ChannelTarget) (*Card, error) {
	var errs []string
	for _, a := range ISDRAIDs {
		ch, err := h.OpenChannel(ctx, target, a.AID)
		if err != nil {
			errs = append(errs, a.Name+": "+err.Error())
			continue
		}
		return &Card{ch: ch, AIDName: a.Name, AID: a.AID}, nil
	}
	return nil, fmt.Errorf("esim: no ISD-R found (not an eUICC?): %s", strings.Join(errs, "; "))
}

// NewReader uses an already authorised channel owned by the caller (for example OMAPI).
// The transport returns response data without status words.
func NewReader(transmit func(context.Context, []byte) ([]byte, error), aidName string) *Card {
	return &Card{transmit: transmit, AIDName: aidName}
}

// Close releases the logical channel.
func (c *Card) Close(ctx context.Context) error {
	if c.ch == nil {
		return nil
	}
	return c.ch.Close(ctx)
}

// es10 sends one ES10 command via STORE DATA (GP 2.3, SGP.22 §5.7) and
// returns the response.
func (c *Card) es10(ctx context.Context, cmd []byte) ([]byte, error) {
	var resp []byte
	for block := 0; len(cmd) > 0; block++ {
		n := len(cmd)
		if n > 255 {
			n = 255
		}
		p1 := byte(0x11)
		if n == len(cmd) {
			p1 = 0x91
		}
		apdu := append([]byte{0x80, 0xE2, p1, byte(block), byte(n)}, cmd[:n]...)
		apdu = append(apdu, 0x00)
		send := c.transmit
		if send == nil {
			send = c.ch.Transmit
		}
		r, err := send(ctx, apdu)
		if err != nil {
			return nil, err
		}
		resp = append(resp, r...)
		cmd = cmd[n:]
	}
	return resp, nil
}

func (c *Card) call(ctx context.Context, cmd []byte, want uint32) (TLV, error) {
	raw, err := c.es10(ctx, cmd)
	if err != nil {
		return TLV{}, err
	}
	tlvs, err := ParseTLVs(raw)
	if err != nil || len(tlvs) == 0 {
		return TLV{}, fmt.Errorf("esim: undecodable response (%d bytes)", len(raw))
	}
	if tlvs[0].Tag != want {
		return TLV{}, fmt.Errorf("esim: unexpected response tag %X", tlvs[0].Tag)
	}
	return tlvs[0], nil
}

// EID reads the eUICC identifier (GetEuiccData).
func (c *Card) EID(ctx context.Context) (string, error) {
	t, err := c.call(ctx, []byte{0xBF, 0x3E, 0x03, 0x5C, 0x01, 0x5A}, 0xBF3E)
	if err != nil {
		return "", err
	}
	if e := t.Find(0x5A); e != nil {
		return strings.ToUpper(hex.EncodeToString(e.Value)), nil
	}
	return "", errors.New("esim: EID missing")
}

// Info is the parsed EUICCInfo2 plus configured addresses.
type Info struct {
	EID              string   `json:"eid"`
	ISDR             string   `json:"isdr"`
	ProfileVersion   string   `json:"profileVersion,omitempty"`
	SGP22Version     string   `json:"sgp22Version,omitempty"`
	FirmwareVersion  string   `json:"firmwareVersion,omitempty"`
	GlobalPlatform   string   `json:"globalPlatform,omitempty"`
	InstalledApps    int      `json:"installedApps,omitempty"`
	FreeNVM          int      `json:"freeNvm,omitempty"`
	FreeVolatile     int      `json:"freeVolatile,omitempty"`
	SASAccreditation string   `json:"sasAccreditation,omitempty"`
	PlatformLabel    string   `json:"platformLabel,omitempty"`
	CIKeyIDs         []string `json:"ciKeyIds,omitempty"`
	DefaultSMDP      string   `json:"defaultSmdp,omitempty"`
	RootSMDS         string   `json:"rootSmds,omitempty"`
	Manufacturer     string   `json:"manufacturer,omitempty"`
}

func version(b []byte) string {
	if len(b) != 3 {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%d.%d.%d", b[0], b[1], b[2])
}

func beInt(b []byte) int {
	n := 0
	for _, x := range b {
		n = n<<8 | int(x)
	}
	return n
}

// Known GSMA CI public key identifiers.
var ciNames = map[string]string{
	"81370f5125d0b1d408d4c3b232e6d25e795bebfb": "GSMA CI (prod)",
	"f54172bdf98a95d65cbeb88a38a1c11d800a85c3": "GSMA test CI",
	"c0bc70ba36929d43b467ff57570530e57ab8fcd8": "GSMA CI2",
}

// ReadInfo collects EID, EUICCInfo2 and configured addresses.
func (c *Card) ReadInfo(ctx context.Context) (Info, error) {
	info := Info{ISDR: c.AIDName}
	var err error
	if info.EID, err = c.EID(ctx); err != nil {
		return info, err
	}
	info.Manufacturer = manufacturerFromEID(info.EID)
	if t, err := c.call(ctx, []byte{0xBF, 0x22, 0x00}, 0xBF22); err == nil {
		for _, k := range t.Children {
			switch k.Tag {
			case 0x81:
				info.ProfileVersion = version(k.Value)
			case 0x82:
				info.SGP22Version = version(k.Value)
			case 0x83:
				info.FirmwareVersion = version(k.Value)
			case 0x87:
				info.GlobalPlatform = version(k.Value)
			case 0x84:
				if res, err := ParseTLVs(k.Value); err == nil {
					for _, r := range res {
						switch r.Tag {
						case 0x81:
							info.InstalledApps = beInt(r.Value)
						case 0x82:
							info.FreeNVM = beInt(r.Value)
						case 0x83:
							info.FreeVolatile = beInt(r.Value)
						}
					}
				}
			case 0x0C:
				info.SASAccreditation = string(k.Value)
			case 0xA9:
				for _, id := range k.Children {
					h := hex.EncodeToString(id.Value)
					if n := ciNames[h]; n != "" {
						h = n + " (" + h[:8] + "…)"
					}
					info.CIKeyIDs = append(info.CIKeyIDs, h)
				}
			case 0xAC:
				if l := k.Find(0x80); l != nil {
					info.PlatformLabel = string(l.Value)
				}
			}
		}
	}
	if t, err := c.call(ctx, []byte{0xBF, 0x3C, 0x00}, 0xBF3C); err == nil {
		if a := t.Find(0x80); a != nil {
			info.DefaultSMDP = string(a.Value)
		}
		if a := t.Find(0x81); a != nil {
			info.RootSMDS = string(a.Value)
		}
	}
	return info, nil
}

// manufacturerFromEID gives a hint from the EUM prefix (digits 9-13 hold the
// manufacturer identifier in the GSMA EID allocation).
func manufacturerFromEID(eid string) string {
	if len(eid) < 32 {
		return ""
	}
	known := map[string]string{
		"89044": "Giesecke+Devrient", "89049": "Thales/Gemalto", "89033": "Thales/Gemalto",
		"89001": "Idemia", "89086": "Kigen/ARM", "89043": "Valid", "89035": "Workz",
		"89034": "Eastcompeace", "89030": "Valid",
	}
	if m, ok := known[eid[:5]]; ok {
		return m
	}
	return ""
}

// Profile is one installed profile.
type Profile struct {
	ICCID        string `json:"iccid"`
	AID          string `json:"aid,omitempty"`
	Enabled      bool   `json:"enabled"`
	Nickname     string `json:"nickname,omitempty"`
	ProviderName string `json:"provider,omitempty"`
	ProfileName  string `json:"name,omitempty"`
	Class        string `json:"class,omitempty"`
}

func decodeICCID(b []byte) string {
	var sb strings.Builder
	for _, x := range b {
		for _, d := range []byte{x & 0x0F, x >> 4} {
			if d <= 9 {
				sb.WriteByte('0' + d)
			}
		}
	}
	return sb.String()
}

func encodeICCID(s string) ([]byte, error) {
	if len(s) < 18 || len(s) > 20 {
		return nil, errors.New("esim: bad ICCID")
	}
	for len(s) < 20 {
		s += "F"
	}
	out := make([]byte, 10)
	for i := 0; i < 10; i++ {
		lo, hi := hexNibble(s[2*i]), hexNibble(s[2*i+1])
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c == 'F' || c == 'f':
		return 0xF
	}
	return 0xF
}

// Profiles lists installed profiles (ProfileInfoListRequest).
func (c *Card) Profiles(ctx context.Context) ([]Profile, error) {
	t, err := c.call(ctx, []byte{0xBF, 0x2D, 0x00}, 0xBF2D)
	if err != nil {
		return nil, err
	}
	list := t.Find(0xA0)
	if list == nil {
		if e := t.Find(0x81); e != nil {
			return nil, fmt.Errorf("esim: profile list error %d", beInt(e.Value))
		}
		return nil, nil
	}
	var out []Profile
	for _, e := range list.Children {
		if e.Tag != 0xE3 {
			continue
		}
		var p Profile
		for _, f := range e.Children {
			switch f.Tag {
			case 0x5A:
				p.ICCID = decodeICCID(f.Value)
			case 0x4F:
				p.AID = strings.ToUpper(hex.EncodeToString(f.Value))
			case 0x9F70:
				p.Enabled = beInt(f.Value) == 1
			case 0x90:
				p.Nickname = string(f.Value)
			case 0x91:
				p.ProviderName = string(f.Value)
			case 0x92:
				p.ProfileName = string(f.Value)
			case 0x95:
				p.Class = map[int]string{0: "test", 1: "provisioning", 2: "operational"}[beInt(f.Value)]
			}
		}
		out = append(out, p)
	}
	return out, nil
}

var profileResults = map[int]string{
	0: "ok", 1: "ICCID/AID not found", 2: "profile in wrong state", 3: "disallowed by policy",
	4: "wrong profile re-enabling", 5: "card busy (CAT)", 6: "undefined", 127: "undefined error",
}

func (c *Card) profileOp(ctx context.Context, tag uint32, iccid string, extra []byte) error {
	raw, err := encodeICCID(iccid)
	if err != nil {
		return err
	}
	var body []byte
	if tag == 0xBF33 { // DeleteProfile takes the identifier directly
		body = EncodeTLV(0x5A, raw)
	} else {
		body = append(EncodeTLV(0xA0, EncodeTLV(0x5A, raw)), extra...)
	}
	t, err := c.call(ctx, EncodeTLV(tag, body), tag)
	if err != nil {
		return err
	}
	if r := t.Find(0x80); r != nil {
		if code := beInt(r.Value); code != 0 {
			return fmt.Errorf("esim: %s", profileResults[code])
		}
	}
	return nil
}

// Enable switches to a profile (the card refreshes; the modem re-reads it).
func (c *Card) Enable(ctx context.Context, iccid string) error {
	return c.profileOp(ctx, 0xBF31, iccid, EncodeTLV(0x81, []byte{0xFF}))
}

// Disable disables a profile.
func (c *Card) Disable(ctx context.Context, iccid string) error {
	return c.profileOp(ctx, 0xBF32, iccid, EncodeTLV(0x81, []byte{0xFF}))
}

// Delete removes a disabled profile permanently.
func (c *Card) Delete(ctx context.Context, iccid string) error {
	return c.profileOp(ctx, 0xBF33, iccid, nil)
}

// SetNickname sets a profile nickname.
func (c *Card) SetNickname(ctx context.Context, iccid, name string) error {
	raw, err := encodeICCID(iccid)
	if err != nil {
		return err
	}
	body := append(EncodeTLV(0x5A, raw), EncodeTLV(0x90, []byte(name))...)
	t, err := c.call(ctx, EncodeTLV(0xBF29, body), 0xBF29)
	if err != nil {
		return err
	}
	if r := t.Find(0x80); r != nil && beInt(r.Value) != 0 {
		return fmt.Errorf("esim: set nickname failed (%d)", beInt(r.Value))
	}
	return nil
}
