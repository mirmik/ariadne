package quictransport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/transport"
)

func TestQUICMessageConnectionAndCertificatePin(t *testing.T) {
	certificatePath, keyPath, certificateDER := testCertificate(t)
	server, err := Listen("127.0.0.1:0", certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	serverContext, cancelServer := context.WithCancel(context.Background())
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.Serve(serverContext, func(connection messageconn.Conn) {
			defer connection.CloseNow()
			messageType, payload, err := connection.Read(serverContext)
			if err != nil {
				return
			}
			if err := connection.Write(serverContext, messageType, append([]byte("echo:"), payload...)); err != nil {
				return
			}
			_, _, _ = connection.Read(serverContext)
		})
	}()
	defer func() {
		cancelServer()
		_ = server.Close()
		<-serverErrors
	}()

	tlsConfig, err := transport.ClientTLSConfig("127.0.0.1", transport.FormatCertificatePin(certificateDER))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := Dial(ctx, server.Addr(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	if err := connection.Write(ctx, websocket.MessageBinary, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageBinary || string(payload) != "echo:hello" {
		t.Fatalf("type=%v payload=%q", messageType, payload)
	}

	wrongTLSConfig, err := transport.ClientTLSConfig("127.0.0.1", transport.FormatCertificatePin([]byte("wrong")))
	if err != nil {
		t.Fatal(err)
	}
	wrongContext, cancelWrong := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWrong()
	if wrongConnection, err := Dial(wrongContext, server.Addr(), wrongTLSConfig); err == nil {
		wrongConnection.CloseNow()
		t.Fatal("QUIC relay accepted the wrong certificate pin")
	}
}

func testCertificate(t *testing.T) (string, string, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Ariadne QUIC test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certificatePath := filepath.Join(directory, "relay.crt")
	keyPath := filepath.Join(directory, "relay.key")
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificatePath, keyPath, certificateDER
}
