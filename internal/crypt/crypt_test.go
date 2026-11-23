package crypt

import (
	"encoding/base64"
	"strings"
	"testing"
)

func key() string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) }

func TestRoundTrip(t *testing.T) {
	b, err := New(key())
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := b.Seal("hunter2")
	if strings.Contains(string(sealed), "hunter2") {
		t.Fatal("plaintext visible in ciphertext")
	}
	got, err := b.Open(sealed)
	if err != nil || got != "hunter2" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestBadKeyAndTamper(t *testing.T) {
	if _, err := New("short"); err == nil {
		t.Fatal("expected error for bad key")
	}
	b, _ := New(key())
	sealed, _ := b.Seal("x")
	sealed[len(sealed)-1] ^= 1
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("expected tamper error")
	}
}
