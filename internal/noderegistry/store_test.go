package noderegistry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistryBoundsAnonymousIdentitiesAndRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, id := range []string{"claimed", "revoked"} {
		if _, err := store.Observe(Observation{NodeID: id, PublicKey: id}, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Claim("claimed", "phone", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke("revoked", now); err != nil {
		t.Fatal(err)
	}
	for i := range MaxUnclaimedNodes {
		id := fmt.Sprintf("anonymous-%d", i)
		if _, err := store.Observe(Observation{NodeID: id, PublicKey: id}, now.Add(time.Duration(i+1)*time.Second)); err != nil {
			t.Fatalf("observation %d: %v", i, err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		id := fmt.Sprintf("excess-%d", i)
		if _, err := store.Observe(Observation{NodeID: id, PublicKey: id}, now.Add(time.Hour)); !errors.Is(err, ErrCapacity) {
			t.Fatalf("quota error: %v", err)
		}
	}
	for filename, want := range map[string][]byte{path: before, path + ".bak": backup} {
		got, err := os.ReadFile(filename)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("rejection changed %s: %v", filename, err)
		}
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.state.Nodes) != MaxUnclaimedNodes+2 || reopened.Claims()["claimed"] != "phone" {
		t.Fatal("lost identities or claim")
	}
	if _, err := reopened.Observe(Observation{NodeID: "revoked", PublicKey: "revoked"}, now); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revocation lost: %v", err)
	}
	if _, err := reopened.Claim("anonymous-0", "new-phone", now); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Observe(Observation{NodeID: "new-node", PublicKey: "new-key"}, now); err != nil {
		t.Fatalf("claim did not free an unclaimed slot: %v", err)
	}
}

func TestRegistryRateLimitsExistingIdentityRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	observation := Observation{NodeID: "node", PublicKey: "key"}
	for range 16 {
		if _, err := store.Observe(observation, now); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Observe(observation, now); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rate rejection changed registry")
	}
	if _, err := store.Claim("node", "phone", now); err != nil {
		t.Fatalf("rate limiter blocked management: %v", err)
	}
	if _, err := store.Observe(observation, now.Add(time.Second)); err != nil {
		t.Fatalf("registration budget did not refill: %v", err)
	}
}

func TestOversizeCommitPreservesPrimaryAndBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := store.Observe(Observation{NodeID: "node", PublicKey: "key"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim("node", "phone", now); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	backup, _ := os.ReadFile(path + ".bak")
	next := cloneState(store.state)
	next.Nodes["huge"] = Record{NodeID: "huge", PublicKey: strings.Repeat("x", maxStateSize)}
	if err := store.commit(next); err == nil {
		t.Fatal("oversized state accepted")
	}
	for filename, want := range map[string][]byte{path: before, path + ".bak": backup} {
		got, err := os.ReadFile(filename)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("failed commit changed %s: %v", filename, err)
		}
	}
	if _, found := store.state.Nodes["huge"]; found {
		t.Fatal("failed commit published in-memory state")
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("cannot restart: %v", err)
	}
}

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
