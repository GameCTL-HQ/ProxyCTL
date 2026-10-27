package main

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
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

func TestParseTokenRejectsOtherHMACAlgorithms(t *testing.T) {
	a := testAuthn()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS384, jwt.MapClaims{
		"sub": "alice",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString(a.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.parseToken(signed); err == nil {
		t.Fatal("parseToken accepted HS384; only HS256 is allowed")
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
		":8080":          false,
		"0.0.0.0:8080":   false,
		"[::]:8080":      false,
		"10.0.0.5:80":    false,
		"10.0.0.10":   false,
	}
	for addr, want := range cases {
		if got := addrIsLoopback(addr); got != want {
			t.Errorf("addrIsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestAuthStateDoesNotDiscloseUsername(t *testing.T) {
	store := NewAuthStore("proxyctl", "proxyctl-auth")
	store.users = []AuthUser{{Username: "secret-admin"}}
	a := &authn{store: store, jwtKey: []byte("unit-test-signing-key-0123456789")}
	rr := httptest.NewRecorder()
	a.state(rr, httptest.NewRequest(http.MethodGet, "/api/auth/state", nil))
	if strings.Contains(rr.Body.String(), "secret-admin") || strings.Contains(rr.Body.String(), "username") {
		t.Fatalf("public auth state disclosed username: %s", rr.Body.String())
	}
}

func TestSetupRejectsOversizedBody(t *testing.T) {
	a := &authn{setupMode: true, bootstrap: "deadbeefcafe", store: NewAuthStore("proxyctl", "proxyctl-auth")}
	body := `{"bootstrapToken":"deadbeefcafe","username":"admin","password":"` + strings.Repeat("x", maxAuthBodyBytes) + `"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(body))
	a.setup(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusRequestEntityTooLarge, rr.Body.String())
	}
}

type blockingAuthBody struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingAuthBody) Read(_ []byte) (int, error) {
	select {
	case <-b.started:
	default:
		close(b.started)
	}
	<-b.release
	return 0, io.EOF
}

func TestSetupDoesNotHoldClaimLockWhileReadingBody(t *testing.T) {
	a := &authn{setupMode: true, bootstrap: "deadbeefcafe", store: NewAuthStore("proxyctl", "proxyctl-auth")}
	body := &blockingAuthBody{started: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", body)
	done := make(chan struct{})
	go func() {
		a.setup(httptest.NewRecorder(), req)
		close(done)
	}()
	<-body.started
	if !a.setupClaimMu.TryLock() {
		close(body.release)
		<-done
		t.Fatal("setup claim lock was held while an unauthenticated body was still being read")
	}
	a.setupClaimMu.Unlock()
	close(body.release)
	<-done
}

func TestLoginClientIPUsesProxyAddressHeaders(t *testing.T) {
	tests := []struct {
		name, header, value, want string
	}{
		{name: "cloudflare", header: "CF-Connecting-IP", value: "203.0.113.10", want: "203.0.113.10"},
		{name: "forwarded", header: "X-Forwarded-For", value: "198.51.100.8, 10.45.0.12", want: "198.51.100.8"},
		{name: "real ip", header: "X-Real-IP", value: "192.0.2.7", want: "192.0.2.7"},
		{name: "invalid falls back", header: "CF-Connecting-IP", value: "not-an-ip", want: "10.45.0.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/auth/token", nil)
			r.RemoteAddr = "10.45.0.9:4321"
			r.Header.Set(tt.header, tt.value)
			if got := loginClientIP(r); got != tt.want {
				t.Fatalf("loginClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoginGuardLimitsRepeatedFailuresPerRemoteIP(t *testing.T) {
	a := &authn{}
	for i := 0; i < maxLoginFailures; i++ {
		finish, ok := a.beginLogin("192.0.2.10:1234")
		if !ok {
			t.Fatalf("attempt %d was limited too early", i+1)
		}
		finish(false)
	}
	if finish, ok := a.beginLogin("192.0.2.10:5678"); ok {
		finish(false)
		t.Fatal("login guard accepted an IP after the failure limit")
	}
	if finish, ok := a.beginLogin("192.0.2.11:1234"); !ok {
		t.Fatal("login guard incorrectly limited a different IP")
	} else {
		finish(true)
	}
}

func TestLoginGuardPrunesExpiredIPEntries(t *testing.T) {
	a := &authn{}
	finish, ok := a.beginLogin("192.0.2.20:1234")
	if !ok {
		t.Fatal("initial login attempt was unexpectedly limited")
	}
	finish(false)
	a.loginMu.Lock()
	a.loginByIP["192.0.2.20"] = loginFailures{started: time.Now().Add(-2 * loginFailureWindow), count: 1}
	a.loginMu.Unlock()

	finish, ok = a.beginLogin("192.0.2.21:1234")
	if !ok {
		t.Fatal("unrelated login attempt was unexpectedly limited")
	}
	finish(true)
	a.loginMu.Lock()
	_, stalePresent := a.loginByIP["192.0.2.20"]
	a.loginMu.Unlock()
	if stalePresent {
		t.Fatal("expired per-IP login state was not pruned")
	}
}
