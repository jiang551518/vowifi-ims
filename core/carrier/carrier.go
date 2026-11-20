// Package carrier holds per-operator VoWiFi parameters. Everything has a
// standards-based default (TS 23.003 ePDG FQDN, SIP URIs with user=phone);
// entries only record what differs or has been verified. Users can override
// every field from the app settings.
package carrier

import (
	"strings"

	"vowifi/ike"
)

// Profile is the effective configuration for one subscription.
type Profile struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	PLMNs       []string           `json:"plmns,omitempty"`
	SPNs        []string           `json:"spns,omitempty"`
	ICCIDPrefix []string           `json:"iccidPrefix,omitempty"`
	EPDG        string             `json:"epdg,omitempty"` // FQDN override
	APN         string             `json:"apn,omitempty"`
	DialScheme  string             `json:"dialScheme,omitempty"` // sip | tel
	UserEqPhone bool               `json:"userEqPhone"`
	SecAgree    bool               `json:"secAgree"`
	PreferTCP   bool               `json:"preferTcp"`
	DeviceID    bool               `json:"deviceIdentity"`
	Proposals   ike.ProposalConfig `json:"proposals,omitempty"`
	CountryCode string             `json:"countryCode,omitempty"`
	// Identity / access overrides for networks that deviate from TS 23.003
	// derivation (empty = derive from IMSI/MCC/MNC or read from the ISIM).
	IMSDomain string   `json:"imsDomain,omitempty"` // home domain / registrar
	IMPIRealm string   `json:"impiRealm,omitempty"` // realm of the private identity
	NAIRealm  string   `json:"naiRealm,omitempty"`  // realm of the SWu NAI
	PCSCF     []string `json:"pcscf,omitempty"`     // P-CSCF addresses when the ePDG gives none
	PANI      string   `json:"pani,omitempty"`      // P-Access-Network-Info
	Verified  bool     `json:"verified"`
	Notes     string   `json:"notes,omitempty"`
}

var profiles = []Profile{
	// MVNOs on the EE network share PLMN 234-33 (and even ICCID prefixes), so
	// they are told apart by the operator name the SIM / Android reports.
	{ID: "ctexcel", Name: "CTExcel（中国电信英国）", PLMNs: []string{"23433", "23430"}, SPNs: []string{"CTExcel"},
		DialScheme: "sip", UserEqPhone: true, SecAgree: true, PreferTCP: true, DeviceID: true, Verified: true,
		Notes: "MVNO on EE; IPv6-only inner address; IMS domain tslt.mvno.ims.ee.co.uk"},
	{ID: "cmlink-uk", Name: "CMLink UK（中国移动英国）", PLMNs: []string{"23433", "23430"}, SPNs: []string{"CMLink"},
		DialScheme: "sip", UserEqPhone: true, SecAgree: true, PreferTCP: true, DeviceID: true,
		Notes: "MVNO on EE; same ePDG as EE"},
	{ID: "ee", Name: "EE", PLMNs: []string{"23433", "23430"},
		DialScheme: "sip", UserEqPhone: true, SecAgree: true, PreferTCP: true, DeviceID: true},
	{ID: "3uk", Name: "Three UK", PLMNs: []string{"23420", "23494"}, DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true,
		Verified: true, Notes: "verified 2026-11-19: IPv6 P-CSCF, sec-agree hmac-sha-1-96/null, expires 7200"},
	{ID: "vodafone-uk", Name: "Vodafone UK", PLMNs: []string{"23415", "23491"}, DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true},
	{ID: "o2-uk", Name: "O2 UK / giffgaff", PLMNs: []string{"23410", "23402", "23411"}, DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true},
	{ID: "cmhk", Name: "China Mobile Hong Kong / CMLink HK", PLMNs: []string{"45412", "45413"}, DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true},
	{ID: "csl", Name: "csl / 1O1O", PLMNs: []string{"45400", "45402", "45418", "45419", "45410"}, DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true},
	{ID: "3hk", Name: "3 Hong Kong", PLMNs: []string{"45403", "45404"}, DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true},
	{ID: "smartone", Name: "SmarTone", PLMNs: []string{"45406", "45415", "45417"}, DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true},
	{ID: "cuhk", Name: "China Unicom Hong Kong", PLMNs: []string{"45407"}, DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true},
	// Globe does not publish the TS 23.003 ePDG name (NXDOMAIN); its ePDG is
	// weconnect.globe.com.ph. Tunnel + EAP-AKA verified 2026-11-19; IMS then
	// refuses registration from outside the Philippines (403 "Service not
	// allowed in this location").
	{ID: "globe", Name: "Globe Telecom", PLMNs: []string{"51502"}, EPDG: "weconnect.globe.com.ph",
		DialScheme: "sip", UserEqPhone: true, SecAgree: true, PreferTCP: true, DeviceID: true,
		Notes: "ePDG weconnect.globe.com.ph; IMS geo-restricted to PH"},
	{ID: "tmobile-us", Name: "T-Mobile US", PLMNs: []string{"310260", "310160", "310200", "310210", "310220", "310230", "310240", "310250", "310270", "310310", "310490", "310660", "310800"},
		EPDG: "ss.epdg.epc.mnc260.mcc310.pub.3gppnetwork.org", DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true},
	{ID: "att-us", Name: "AT&T", PLMNs: []string{"310410", "310280", "310150", "310170", "310380", "313100"},
		EPDG: "epdg.epc.att.net", DialScheme: "sip", UserEqPhone: true, SecAgree: true, DeviceID: true},
}

// Default returns the profile used for unknown operators.
func Default() Profile {
	return Profile{ID: "standard", Name: "3GPP standard", DialScheme: "sip", UserEqPhone: true, SecAgree: true, PreferTCP: true, DeviceID: true}
}

// Match finds the best profile for a subscription.
func Match(mcc, mnc, spn, iccid string) Profile {
	plmn := mcc + mnc
	best, score := Default(), 0
	for _, p := range profiles {
		s := 0
		for _, x := range p.PLMNs {
			if x == plmn {
				s = 10
			}
		}
		if s == 0 {
			continue
		}
		spnHit := false
		for _, x := range p.SPNs {
			// Android reports names like "中国电信 — CTExcel": match substrings.
			if x != "" && strings.Contains(strings.ToLower(spn), strings.ToLower(x)) {
				s += 5
				spnHit = true
				break
			}
		}
		if len(p.SPNs) > 0 && !spnHit {
			continue // brand-specific profile (an MVNO) that this SIM is not
		}
		for _, x := range p.ICCIDPrefix {
			if strings.HasPrefix(iccid, x) {
				s += 3
			}
		}
		if s > score {
			best, score = p, s
		}
	}
	if best.CountryCode == "" {
		best.CountryCode = CountryCode(mcc)
	}
	return best
}

// All lists the built-in profiles.
func All() []Profile { return append([]Profile(nil), profiles...) }

// EPDGHost returns the ePDG FQDN for a subscription.
func (p Profile) EPDGHost(mcc, mnc string) string {
	if p.EPDG != "" {
		return p.EPDG
	}
	return ike.EPDGHost(mcc, mnc)
}

// CountryCode maps an MCC to its E.164 country code (common ones).
func CountryCode(mcc string) string {
	return map[string]string{
		"202": "30", "204": "31", "206": "32", "208": "33", "214": "34", "216": "36", "222": "39",
		"226": "40", "228": "41", "230": "420", "232": "43", "234": "44", "235": "44", "238": "45",
		"240": "46", "242": "47", "244": "358", "246": "370", "247": "371", "248": "372", "250": "7",
		"255": "380", "260": "48", "262": "49", "268": "351", "270": "352", "272": "353", "274": "354",
		"276": "355", "278": "356", "280": "357", "284": "359", "286": "90", "293": "386", "294": "389",
		"297": "382", "302": "1", "310": "1", "311": "1", "312": "1", "313": "1", "316": "1",
		"334": "52", "404": "91", "405": "91", "410": "92", "413": "94", "414": "95", "415": "961",
		"416": "962", "418": "964", "419": "965", "420": "966", "424": "971", "425": "972", "427": "974",
		"440": "81", "441": "81", "450": "82", "452": "84", "454": "852", "455": "853", "456": "855",
		"457": "856", "460": "86", "466": "886", "502": "60", "505": "61", "510": "62", "515": "63",
		"520": "66", "525": "65", "530": "64", "602": "20", "655": "27", "724": "55", "722": "54",
		"730": "56", "732": "57", "716": "51",
	}[mcc]
}
