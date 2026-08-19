package transport

import "testing"

func TestValidateRelayURLRequiresTLSOutsideLoopback(t *testing.T) {
	tests := []struct {
		url       string
		allow     bool
		wantError bool
	}{
		{url: "http://127.0.0.1:8088"},
		{url: "ws://[::1]:8088"},
		{url: "https://relay.example"},
		{url: "wss://relay.example"},
		{url: "http://relay.example", wantError: true},
		{url: "http://relay.example", allow: true},
	}
	for _, test := range tests {
		err := ValidateRelayURL(test.url, test.allow)
		if (err != nil) != test.wantError {
			t.Fatalf("ValidateRelayURL(%q, %v) error=%v, wantError=%v", test.url, test.allow, err, test.wantError)
		}
	}
}

func TestValidateListenAddressRequiresTLSOutsideLoopback(t *testing.T) {
	if err := ValidateListenAddress("127.0.0.1:8088", false, false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateListenAddress("0.0.0.0:8088", false, false); err == nil {
		t.Fatal("public plaintext listener was accepted")
	}
	if err := ValidateListenAddress("0.0.0.0:8088", true, false); err != nil {
		t.Fatal(err)
	}
}
