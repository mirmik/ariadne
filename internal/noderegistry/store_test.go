package noderegistry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistryPersistsClaimRenameAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ariadne", "registry.json")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := Observation{NodeID: "node-one", PublicKey: "key-one", ReportedAlias: "temporary", Platform: "linux"}
	second := Observation{NodeID: "node-two", PublicKey: "key-two", ReportedAlias: "other", Platform: "windows"}
	if _, err := store.Observe(first, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Observe(second, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(first.NodeID, "phone", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(second.NodeID, "PHONE", now.Add(2*time.Minute)); err == nil {
		t.Fatal("case-insensitive alias takeover succeeded")
	}
	if _, err := store.Claim(first.NodeID, "radio", now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Claims()[first.NodeID]; got != "radio" {
		t.Fatalf("persisted alias=%q, want radio", got)
	}
	if _, err := reopened.Observe(Observation{NodeID: first.NodeID, PublicKey: "different-key"}, now); err == nil {
		t.Fatal("registry accepted a changed public key")
	}
	if _, err := reopened.Revoke(first.NodeID, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, found := reopened.Claims()[first.NodeID]; found {
		t.Fatal("revoked node retained active alias ownership")
	}
	if _, err := reopened.Claim(second.NodeID, "radio", now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Observe(first, now.Add(6*time.Minute)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked identity observation error=%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("registry permissions=%v", info.Mode().Perm())
	}
}

func TestRegistryRecoversMissingPrimaryFromBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	observation := Observation{NodeID: "node-one", PublicKey: "key-one", ReportedAlias: "node"}
	if _, err := store.Observe(observation, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(observation.NodeID, "phone", now); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.Claims()[observation.NodeID]; got != "" {
		// The backup intentionally contains the last complete state before the
		// latest transaction, which is the observed identity without its claim.
		t.Fatalf("recovered claim=%q, want previous transaction", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("primary was not restored: %v", err)
	}
}

func TestRegistryRejectsUnsafePermissionsAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte("{\"version\":2,\"nodes\":{}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("unsupported schema version was accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("overly broad permissions were accepted")
	}
}

func TestRegistryDoesNotIgnoreBrokenBackupWhenPrimaryIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path+".bak", []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("missing primary silently ignored a broken backup")
	}
}
