package ike

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"hash"
	"math/big"
)

// verifyResponderSignature checks the certificate-based AUTH that the ePDG
// sends with its first IKE_AUTH response. Mutual authentication of the
// tunnel rests on EAP-AKA (AUTN) and the MSK-based AUTH checked later; this
// signature is reported for diagnostics only. Chain validation against
// carrier roots is not performed. Returns "", "sig", or "sig-invalid".
func (s *Session) verifyResponderSignature(m *Message, msg2, ni, idr []byte) string {
	ap, cp := m.find(PayloadAUTH), m.find(PayloadCERT)
	if ap == nil || cp == nil || len(ap.Body) < 4 || len(cp.Body) < 2 || cp.Body[0] != 4 || idr == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(cp.Body[1:])
	if err != nil {
		return "sig-invalid"
	}
	s.mu.Lock()
	sa := s.sa
	s.mu.Unlock()
	signed := append(append(append([]byte(nil), msg2...), ni...), sa.suite.PRF.prf(sa.skPr, idr)...)
	method, data := ap.Body[0], ap.Body[4:]
	ok := false
	switch method {
	case 1: // RSA PKCS#1 v1.5 with SHA-1
		ok = verifyWith(cert.PublicKey, crypto.SHA1, sha1.New, signed, data, false)
	case 9:
		ok = verifyWith(cert.PublicKey, crypto.SHA256, sha256.New, signed, data, true)
	case 10:
		ok = verifyWith(cert.PublicKey, crypto.SHA384, sha512.New384, signed, data, true)
	case 11:
		ok = verifyWith(cert.PublicKey, crypto.SHA512, sha512.New, signed, data, true)
	case 14: // RFC 7427: length-prefixed AlgorithmIdentifier, then signature
		if len(data) < 1 || len(data) < 1+int(data[0]) {
			break
		}
		var alg struct {
			OID    asn1.ObjectIdentifier
			Params asn1.RawValue `asn1:"optional"`
		}
		if _, err := asn1.Unmarshal(data[1:1+int(data[0])], &alg); err != nil {
			break
		}
		sig := data[1+int(data[0]):]
		switch alg.OID.String() {
		case "1.2.840.113549.1.1.5":
			ok = verifyWith(cert.PublicKey, crypto.SHA1, sha1.New, signed, sig, false)
		case "1.2.840.113549.1.1.11":
			ok = verifyWith(cert.PublicKey, crypto.SHA256, sha256.New, signed, sig, false)
		case "1.2.840.113549.1.1.12":
			ok = verifyWith(cert.PublicKey, crypto.SHA384, sha512.New384, signed, sig, false)
		case "1.2.840.113549.1.1.13":
			ok = verifyWith(cert.PublicKey, crypto.SHA512, sha512.New, signed, sig, false)
		case "1.2.840.10045.4.3.2":
			ok = verifyWith(cert.PublicKey, crypto.SHA256, sha256.New, signed, sig, false)
		case "1.2.840.10045.4.3.3":
			ok = verifyWith(cert.PublicKey, crypto.SHA384, sha512.New384, signed, sig, false)
		case "1.2.840.10045.4.3.4":
			ok = verifyWith(cert.PublicKey, crypto.SHA512, sha512.New, signed, sig, false)
		case "1.2.840.113549.1.1.10": // RSASSA-PSS, assume SHA-256
			if pub, isRSA := cert.PublicKey.(*rsa.PublicKey); isRSA {
				d := sha256.Sum256(signed)
				ok = rsa.VerifyPSS(pub, crypto.SHA256, d[:], sig, nil) == nil
			}
		}
	}
	if ok {
		s.log.Info("ike: ePDG certificate signature valid", "subject", cert.Subject.String())
		return "sig"
	}
	s.log.Warn("ike: ePDG certificate signature could not be verified", "method", method, "subject", cert.Subject.String())
	return "sig-invalid"
}

func verifyWith(pub any, h crypto.Hash, nh func() hash.Hash, data, sig []byte, rawECDSA bool) bool {
	d := nh()
	d.Write(data)
	sum := d.Sum(nil)
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(k, h, sum, sig) == nil
	case *ecdsa.PublicKey:
		if rawECDSA {
			n := len(sig) / 2
			r, s := new(big.Int).SetBytes(sig[:n]), new(big.Int).SetBytes(sig[n:])
			return ecdsa.Verify(k, sum, r, s)
		}
		return ecdsa.VerifyASN1(k, sum, sig)
	}
	return false
}
