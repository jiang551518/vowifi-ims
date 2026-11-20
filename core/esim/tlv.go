// Package esim reads and manages removable eUICCs (GSMA SGP.22 ES10
// functions over APDUs to the ISD-R): EID, eUICC info and profiles.
package esim

import (
	"errors"
)

// TLV is a decoded BER-TLV node.
type TLV struct {
	Tag      uint32
	Value    []byte
	Children []TLV
}

func (t TLV) constructed() bool {
	first := t.Tag
	for first > 0xFF {
		first >>= 8
	}
	return first&0x20 != 0
}

// ParseTLVs decodes a sequence of BER-TLVs, recursing into constructed ones.
func ParseTLVs(b []byte) ([]TLV, error) {
	var out []TLV
	for len(b) > 0 {
		if b[0] == 0x00 || b[0] == 0xFF { // padding
			b = b[1:]
			continue
		}
		tag := uint32(b[0])
		i := 1
		if b[0]&0x1F == 0x1F {
			for {
				if i >= len(b) {
					return nil, errors.New("tlv: truncated tag")
				}
				tag = tag<<8 | uint32(b[i])
				i++
				if b[i-1]&0x80 == 0 {
					break
				}
			}
		}
		if i >= len(b) {
			return nil, errors.New("tlv: missing length")
		}
		l := int(b[i])
		i++
		if l&0x80 != 0 {
			n := l & 0x7F
			if n == 0 || n > 3 || i+n > len(b) {
				return nil, errors.New("tlv: bad length")
			}
			l = 0
			for k := 0; k < n; k++ {
				l = l<<8 | int(b[i+k])
			}
			i += n
		}
		if i+l > len(b) {
			return nil, errors.New("tlv: value exceeds buffer")
		}
		t := TLV{Tag: tag, Value: b[i : i+l]}
		if t.constructed() {
			if kids, err := ParseTLVs(t.Value); err == nil {
				t.Children = kids
			}
		}
		out = append(out, t)
		b = b[i+l:]
	}
	return out, nil
}

// Find returns the first direct child with tag.
func (t TLV) Find(tag uint32) *TLV {
	for i := range t.Children {
		if t.Children[i].Tag == tag {
			return &t.Children[i]
		}
	}
	return nil
}

// EncodeTLV builds one TLV.
func EncodeTLV(tag uint32, value []byte) []byte {
	var out []byte
	switch {
	case tag > 0xFFFF:
		out = append(out, byte(tag>>16), byte(tag>>8), byte(tag))
	case tag > 0xFF:
		out = append(out, byte(tag>>8), byte(tag))
	default:
		out = append(out, byte(tag))
	}
	l := len(value)
	switch {
	case l < 0x80:
		out = append(out, byte(l))
	case l < 0x100:
		out = append(out, 0x81, byte(l))
	default:
		out = append(out, 0x82, byte(l>>8), byte(l))
	}
	return append(out, value...)
}
