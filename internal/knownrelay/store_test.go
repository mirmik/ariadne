package knownrelay

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStoreLearnsRejectsAndExplicitlyReplacesCertificate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ariadne", "known_relays")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	firstCertificate := []byte("first certificate")
	first, err := store.Verify("relay.example:47471", firstCertificate, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Decision != Learned {
		t.Fatalf("first decision=%v, want Learned", first.Decision)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("known relays permissions=%04o, want 0600", got)
		}
	}

	known, err := store.Verify("relay.example:47471", firstCertificate, false)
	if err != nil {
		t.Fatal(err)
	}
	if known.Decision != Known {
		t.Fatalf("second decision=%v, want Known", known.Decision)
	}

	secondCertificate := []byte("second certificate")
	_, err = store.Verify("relay.example:47471", secondCertificate, false)
	var changed *CertificateChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("mismatch error=%v, want CertificateChangedError", err)
	}
	if changed.KnownPin != first.Pin || changed.ReceivedPin == first.Pin {
		t.Fatalf("unexpected changed certificate details: %#v", changed)
	}

	replaced, err := store.Verify("relay.example:47471", secondCertificate, true)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Decision != Replaced || replaced.PreviousPin != first.Pin {
		t.Fatalf("replacement=%#v", replaced)
	}

	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := reloaded.Verify("relay.example:47471", secondCertificate, false); err != nil || result.Decision != Known {
		t.Fatalf("reloaded verification result=%#v error=%v", result, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(data), "relay.example:47471 "); count != 2 {
		t.Fatalf("relay history contains %d entries, want 2:\n%s", count, data)
	}
}

func TestOpenRejectsBroadPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "known_relays")
	if err := os.WriteFile(path, []byte("relay.example:47471 sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("Open error=%v, want permissions error", err)
	}
}

func TestOpenRejectsMalformedEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_relays")
	if err := os.WriteFile(path, []byte("not enough fields here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "expected endpoint") {
		t.Fatalf("Open error=%v, want parse error", err)
	}
}
