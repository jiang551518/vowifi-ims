package ims

import "testing"

// An SMS-PP data download (TP-PID 0x7F, class 2, UDH IEI 0x70) is OTA data
// for the SIM, not a user text.
func TestParseDeliverSIMDataDownload(t *testing.T) {
	ud := []byte{0x02, 0x70, 0x00, 0x01, 0x20, 0x15, 0x16, 0x00, 0x25, 0x25, 0xb0, 0x00, 0x10}
	tp := []byte{0x40, // SMS-DELIVER, UDHI
		0x05, 0x81, 0x21, 0x43, 0xf5, // OA "12345"
		0x7f, 0xf6, // PID SIM data download, DCS class 2 8-bit
		0x62, 0x11, 0x91, 0x32, 0x20, 0x00, 0x00, // SCTS
		byte(len(ud))}
	tp = append(tp, ud...)
	d, err := parseDeliver(tp)
	if err != nil {
		t.Fatal(err)
	}
	if !d.simData || d.pid != 0x7f || d.class != 2 {
		t.Fatalf("simData=%v pid=%02X class=%d", d.simData, d.pid, d.class)
	}

	// A normal 8-bit class-2 text without the OTA header stays a text.
	plain := []byte{0x00, 0x05, 0x81, 0x21, 0x43, 0xf5, 0x00, 0xf6,
		0x62, 0x11, 0x91, 0x32, 0x20, 0x00, 0x00, 0x02, 'h', 'i'}
	if d, err := parseDeliver(plain); err != nil || d.simData || d.text != "hi" {
		t.Fatalf("plain: %+v %v", d, err)
	}
}
