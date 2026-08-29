package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestCertificatePinParsingAndFormatting(t *testing.T) {
	certificate := []byte("certificate")
	digest := sha256.Sum256(certificate)
	formatted := FormatCertificatePin(certificate)
	parsed, err := ParseCertificatePin(formatted)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(parsed) != hex.EncodeToString(digest[:]) {
		t.Fatalf("pin=%x want=%x", parsed, digest)
	}
	if _, err := ParseCertificatePin("sha256:abcd"); err == nil {
		t.Fatal("short certificate pin was accepted")
	}
}

func TestNormalizeCertificatePin(t *testing.T) {
	const digest = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	normalized, err := NormalizeCertificatePin("SHA256:" + digest)
	if err != nil {
		t.Fatal(err)
	}
	if normalized != "sha256:"+digest {
		t.Fatalf("normalized pin=%q", normalized)
	}
	if _, err := NormalizeCertificatePin(""); err == nil {
		t.Fatal("empty pin was normalized")
	}
}
