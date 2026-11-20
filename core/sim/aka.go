package sim

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// UICC application types and authentication types used by
// TelephonyManager.getIccAuthentication.
const (
	AppUSIM = 2
	AppISIM = 5

	AuthEAPSIM = 128
	AuthEAPAKA = 129
)

// ErrSyncFailure is returned with an AKAResult carrying AUTS when the SIM
// rejects the sequence number in AUTN (resynchronisation required).
var ErrSyncFailure = errors.New("sim: AKA synchronisation failure")

// AKAResult is the outcome of one UMTS AKA run (3GPP TS 31.102 §7.1.2).
type AKAResult struct {
	RES  []byte
	CK   []byte
	IK   []byte
	AUTS []byte
}

// AKA runs AUTHENTICATE on the USIM or ISIM application of a subscription.
// It returns ErrSyncFailure (with AUTS populated) for resync responses. A
// MAC failure on AUTN surfaces as an error from the card.
func (h *Helper) AKA(ctx context.Context, subID, appType int, rand, autn []byte) (AKAResult, error) {
	if len(rand) != 16 || len(autn) != 16 {
		return AKAResult{}, errors.New("sim: RAND and AUTN must be 16 bytes")
	}
	payload := make([]byte, 0, 34)
	payload = append(payload, byte(len(rand)))
	payload = append(payload, rand...)
	payload = append(payload, byte(len(autn)))
	payload = append(payload, autn...)
	var resp struct {
		Result string `json:"result"`
	}
	err := h.Call(ctx, "auth", map[string]any{
		"subId":    subID,
		"appType":  appType,
		"authType": AuthEAPAKA,
		"data":     base64.StdEncoding.EncodeToString(payload),
	}, &resp)
	if err != nil {
		return AKAResult{}, err
	}
	raw, err := decodeLoose(resp.Result)
	if err != nil {
		return AKAResult{}, fmt.Errorf("sim: decode AKA response: %w", err)
	}
	return ParseAKAResponse(raw)
}

// decodeLoose accepts base64 (Android API contract) and, defensively, hex.
func decodeLoose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return hex.DecodeString(s)
}

// ParseAKAResponse decodes the AUTHENTICATE response data object.
//
//	DB L RES L CK L IK [L Kc]  -> success
//	DC L AUTS                  -> synchronisation failure
func ParseAKAResponse(raw []byte) (AKAResult, error) {
	if len(raw) < 2 {
		return AKAResult{}, errors.New("sim: AKA response too short")
	}
	switch raw[0] {
	case 0xDB:
		fields, err := splitLV(raw[1:], 3)
		if err != nil {
			return AKAResult{}, err
		}
		if len(fields[1]) != 16 || len(fields[2]) != 16 {
			return AKAResult{}, errors.New("sim: unexpected CK/IK length")
		}
		return AKAResult{RES: fields[0], CK: fields[1], IK: fields[2]}, nil
	case 0xDC:
		fields, err := splitLV(raw[1:], 1)
		if err != nil {
			return AKAResult{}, err
		}
		return AKAResult{AUTS: fields[0]}, ErrSyncFailure
	default:
		return AKAResult{}, fmt.Errorf("sim: unknown AKA response tag %02X", raw[0])
	}
}

func splitLV(b []byte, n int) ([][]byte, error) {
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		if len(b) < 1 || len(b) < 1+int(b[0]) {
			return nil, errors.New("sim: truncated AKA response")
		}
		l := int(b[0])
		out = append(out, append([]byte(nil), b[1:1+l]...))
		b = b[1+l:]
	}
	return out, nil
}
