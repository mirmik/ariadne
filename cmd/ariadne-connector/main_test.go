package main

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveConnectorAliasUsesExplicitValueUnchanged(t *testing.T) {
	called := false
	alias, hostnameDefault, err := resolveConnectorAlias("custom_alias", func() (string, error) {
		called = true
		return "ignored", nil
	})
	if err != nil || called || hostnameDefault || alias != "custom_alias" {
		t.Fatalf("unexpected explicit alias result: alias=%q default=%v called=%v err=%v", alias, hostnameDefault, called, err)
	}
}

func TestNormalizeHostnameAlias(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{name: "ordinary", hostname: "Aurora", want: "Aurora"},
		{name: "dotted", hostname: "buildbox.example.test", want: "buildbox.example.test"},
		{name: "invalid characters", hostname: "-- weird host !!", want: "weird-host"},
		{name: "leading separators", hostname: ".._phone", want: "phone"},
		{name: "trailing separators", hostname: "phone._-", want: "phone"},
		{name: "overlong", hostname: strings.Repeat("a", 70), want: strings.Repeat("a", 63)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeHostnameAlias(test.hostname); got != test.want {
				t.Fatalf("normalizeHostnameAlias(%q) = %q, want %q", test.hostname, got, test.want)
			}
		})
	}
}

func TestResolveConnectorAliasReportsHostnameFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		hostname func() (string, error)
	}{
		{name: "lookup error", hostname: func() (string, error) { return "", errors.New("unavailable") }},
		{name: "unusable hostname", hostname: func() (string, error) { return "---...___", nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := resolveConnectorAlias("", test.hostname)
			if err == nil || !strings.Contains(err.Error(), "--alias") {
				t.Fatalf("expected actionable alias error, got %v", err)
			}
		})
	}
}

func TestValidatePersistentArguments(t *testing.T) {
	if err := validatePersistentArguments([]string{"--relay", "relay.example", "--alias", "radio room", "--max-exec-timeout", "5m"}); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
	for _, test := range []struct {
		name      string
		arguments []string
		contains  string
	}{
		{name: "unknown flag", arguments: []string{"--typo"}, contains: "flag provided"},
		{name: "positional", arguments: []string{"relay.example"}, contains: "unexpected"},
		{name: "missing relay", arguments: []string{"--alias", "radio"}, contains: "require --relay"},
		{name: "version", arguments: []string{"--version"}, contains: "cannot be saved"},
		{name: "persistent trust override", arguments: []string{"--accept-new-relay-certificate"}, contains: "one-time"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validatePersistentArguments(test.arguments)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("expected error containing %q, got %v", test.contains, err)
			}
		})
	}
}
