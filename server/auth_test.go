package main

import (
	"encoding/hex"
	"testing"
)

func testAuthn() *authn {
	return &authn{jwtKey: []byte("unit-test-signing-key-0123456789")}
}

func TestTokenRoundTrip(t *testing.T) {
	a := testAuthn()
	tok, err := a.issueToken("alice")
	if err != nil {
		t.Fatalf("issueToken: %v", err)
	}
	sub, err := a.parseToken(tok)
	if err != nil {
		t.Fatalf("parseToken: %v", err)
	}
	if sub != "alice" {
		t.Fatalf("sub = %q, want alice", sub)
	}
}

func TestParseTokenRejectsWrongKey(t *testing.T) {
	a := testAuthn()
	tok, _ := a.issueToken("alice")
	other := &authn{jwtKey: []byte("a-totally-different-signing-key!!")}
	if _, err := other.parseToken(tok); err == nil {
		t.Fatal("parseToken accepted a token signed with a different key")
	}
}

func TestParseTokenRejectsGarbage(t *testing.T) {
	a := testAuthn()
	if _, err := a.parseToken("not.a.real.jwt"); err == nil {
		t.Fatal("parseToken accepted a garbage token")
	}
}

func TestIssueTokenNeedsKey(t *testing.T) {
	a := &authn{} // no signing key
	if _, err := a.issueToken("alice"); err == nil {
		t.Fatal("issueToken should fail without a signing key")
	}
}

func TestMatchBootstrap(t *testing.T) {
	a := &authn{setupMode: true, bootstrap: "deadbeefcafe"}
	if !a.matchBootstrap("deadbeefcafe") {
		t.Fatal("matchBootstrap rejected the correct token")
	}
	if a.matchBootstrap("wrong") {
		t.Fatal("matchBootstrap accepted a wrong token")
	}
	a.setupMode = false // once setup ends, never matches
	if a.matchBootstrap("deadbeefcafe") {
		t.Fatal("matchBootstrap matched after setup mode ended")
	}
}

func TestGenBootstrapIsHex32(t *testing.T) {
	tok, err := genBootstrap()
	if err != nil {
		t.Fatalf("genBootstrap: %v", err)
	}
	if len(tok) != 32 {
		t.Fatalf("bootstrap len = %d, want 32", len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Fatalf("bootstrap is not hex: %v", err)
	}
}

func TestAddrIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"localhost":      true,
		"[::1]:443":      true,
		"10.0.0.5:80":    false,
		"10.0.0.10":   false,
	}
	for addr, want := range cases {
		if got := addrIsLoopback(addr); got != want {
			t.Errorf("addrIsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
