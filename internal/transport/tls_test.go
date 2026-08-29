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
