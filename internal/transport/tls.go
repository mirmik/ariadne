package transport

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const CertificatePinSize = sha256.Size

func ParseCertificatePin(value string) ([]byte, error) {
	normalized := strings.TrimSpace(value)
	normalized = strings.TrimPrefix(strings.ToLower(normalized), "sha256:")
	normalized = strings.ReplaceAll(normalized, ":", "")
	if normalized == "" {
		return nil, nil
	}
	pin, err := hex.DecodeString(normalized)
	if err != nil || len(pin) != CertificatePinSize {
		return nil, errors.New("certificate pin must be a 32-byte SHA-256 digest in hexadecimal")
	}
	return pin, nil
}

func FormatCertificatePin(certificateDER []byte) string {
	digest := sha256.Sum256(certificateDER)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func NormalizeCertificatePin(value string) (string, error) {
	pin, err := ParseCertificatePin(value)
	if err != nil {
		return "", err
	}
	if len(pin) == 0 {
		return "", errors.New("certificate pin is empty")
	}
	return "sha256:" + hex.EncodeToString(pin), nil
}

func ClientTLSConfig(serverName, pinValue string) (*tls.Config, error) {
	pin, err := ParseCertificatePin(pinValue)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
	}
	if len(pin) == 0 {
		return config, nil
	}
	return ClientTLSConfigWithLeafVerifier(serverName, func(certificateDER []byte) error {
		digest := sha256.Sum256(certificateDER)
		if subtle.ConstantTimeCompare(digest[:], pin) != 1 {
			return fmt.Errorf("relay TLS certificate pin mismatch: received %s", FormatCertificatePin(certificateDER))
		}
		return nil
	}), nil
}

func ClientTLSConfigWithLeafVerifier(serverName string, verify func([]byte) error) *tls.Config {
	config := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         serverName,
		InsecureSkipVerify: true, // The caller-supplied leaf verifier replaces Web PKI verification.
	}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("relay did not present a TLS certificate")
		}
		return verify(state.PeerCertificates[0].Raw)
	}
	return config
}
