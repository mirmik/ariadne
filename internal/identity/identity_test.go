package identity

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "identity.json")
	first, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first LoadOrCreate did not create an identity")
	}
	second, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second LoadOrCreate replaced the identity")
	}
	if first.NodeID() != second.NodeID() {
		t.Fatalf("node ID changed: %s != %s", first.NodeID(), second.NodeID())
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("identity permissions are %04o, expected 0600", got)
		}
	}
}

func TestSignaturesVerify(t *testing.T) {
	nodeIdentity, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("registration transcript")
	signature, err := ParseSignature(nodeIdentity.Sign(message))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(nodeIdentity.PublicKey(), message, signature) {
		t.Fatal("generated signature did not verify")
	}
	parsed, err := ParsePublicKey(nodeIdentity.EncodedPublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if NodeID(parsed) != nodeIdentity.NodeID() {
		t.Fatal("encoded public key changed the node ID")
	}
}

func TestSSHHostSignerIsStableAndSeparatedFromNodeIdentity(t *testing.T) {
	nodeIdentity, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	first := nodeIdentity.SSHHostSigner().Public().(ed25519.PublicKey)
	second := nodeIdentity.SSHHostSigner().Public().(ed25519.PublicKey)
	if !bytes.Equal(first, second) {
		t.Fatal("derived SSH host key is not stable")
	}
	if bytes.Equal(first, nodeIdentity.PublicKey()) {
		t.Fatal("SSH host key reuses the node identity key")
	}
}
