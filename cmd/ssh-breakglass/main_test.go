package main

import (
	"strings"
	"testing"
)

func TestRandomText(t *testing.T) {
	const alphabet = "abc234"
	value, err := randomText(128, alphabet)
	if err != nil {
		t.Fatal(err)
	}
	if len(value) != 128 {
		t.Fatalf("length=%d, want 128", len(value))
	}
	for _, character := range value {
		if !strings.ContainsRune(alphabet, character) {
			t.Fatalf("unexpected character %q", character)
		}
	}
}

func TestRandomTextRejectsInvalidParameters(t *testing.T) {
	if _, err := randomText(0, "abc"); err == nil {
		t.Fatal("zero length was accepted")
	}
	if _, err := randomText(10, ""); err == nil {
		t.Fatal("empty alphabet was accepted")
	}
}
