package main

// ProxyCTL's auth credential store. Matches GameCTL's auth-Secret model:
// the JWT signing key and the bcrypt-hashed admin record live in a
// Kubernetes Secret (default `proxyctl-auth` in the ProxyCTL namespace),
// not on the PVC. Recovery is `kubectl delete secret proxyctl-auth`
// + rollout restart — same as GameCTL's `kubectl delete secret
// gamectl-auth`. ProxyCTL holds no credential material outside that
// Secret + the per-request in-memory cache populated from it.
//
// Storage backend is `kubectl` shell-out (same as kube.go's picker and
// the SSH applier's `kubectl set image` step) — ProxyCTL never pulls in
// client-go, so its dep surface stays tiny.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// AuthUser is one row of the persisted `users.json` Secret key. Plural for
// shape parity with GameCTL even though ProxyCTL is single-operator and
// only ever stores one.
type AuthUser struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// AuthStore is the in-memory cache + Secret-persistence layer.
type AuthStore struct {
	mu         sync.RWMutex
	namespace  string
	secretName string
	kubectl    string

	jwtKey []byte
	users  []AuthUser
}

func NewAuthStore(namespace, secretName string) *AuthStore {
	return &AuthStore{
		namespace:  namespace,
		secretName: secretName,
		kubectl:    "kubectl",
	}
}

// Load reads the Secret. ok=false means "no admin claimed yet" → caller
// boots into setup mode and generates an ephemeral JWT key + bootstrap
// token in memory until claim completes. A real error (RBAC denied,
// cluster unreachable, etc.) is fatal.
func (s *AuthStore) Load(ctx context.Context) (ok bool, err error) {
	cmd := exec.CommandContext(ctx, s.kubectl, "-n", s.namespace, "get", "secret", s.secretName, "-o", "json")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		stderr := errBuf.String()
		if strings.Contains(stderr, "NotFound") || strings.Contains(stderr, "not found") {
			return false, nil
		}
		return false, fmt.Errorf("kubectl get secret %s/%s: %w (stderr: %s)", s.namespace, s.secretName, err, strings.TrimSpace(stderr))
	}
	var sec struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &sec); err != nil {
		return false, fmt.Errorf("parse secret JSON: %w", err)
	}
	jwtB64 := sec.Data["jwt"]
	usersB64 := sec.Data["users.json"]
	if jwtB64 == "" || usersB64 == "" {
		return false, nil // partial / empty Secret → treat as fresh setup
	}
	jwtKey, err := base64.StdEncoding.DecodeString(jwtB64)
	if err != nil {
		return false, fmt.Errorf("decode jwt: %w", err)
	}
	usersJSON, err := base64.StdEncoding.DecodeString(usersB64)
	if err != nil {
		return false, fmt.Errorf("decode users.json: %w", err)
	}
	var users []AuthUser
	if err := json.Unmarshal(usersJSON, &users); err != nil {
		return false, fmt.Errorf("parse users.json: %w", err)
	}
	if len(users) == 0 {
		return false, nil
	}

	s.mu.Lock()
	s.jwtKey = jwtKey
	s.users = users
	s.mu.Unlock()
	return true, nil
}

// Save persists the JWT key + user records into the Secret idempotently
// (create-or-update via `kubectl apply -f -`). Mirrors GameCTL's
// WriteSecret semantics with `jwt` + `users.json` keys.
func (s *AuthStore) Save(ctx context.Context, jwtKey []byte, users []AuthUser) error {
	usersJSON, err := json.Marshal(users)
	if err != nil {
		return err
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
data:
  jwt: %s
  users.json: %s
`, s.secretName, s.namespace,
		base64.StdEncoding.EncodeToString(jwtKey),
		base64.StdEncoding.EncodeToString(usersJSON),
	)

	cmd := exec.CommandContext(ctx, s.kubectl, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply secret %s/%s: %w (stderr: %s)", s.namespace, s.secretName, err, strings.TrimSpace(errBuf.String()))
	}

	s.mu.Lock()
	s.jwtKey = jwtKey
	s.users = users
	s.mu.Unlock()
	return nil
}

// JWTSecret returns the active signing key. Only meaningful post-Load
// (or post-setup adoption); zero-length while in setup mode.
func (s *AuthStore) JWTSecret() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]byte, len(s.jwtKey))
	copy(out, s.jwtKey)
	return out
}

// Verify constant-time bcrypt-compares the password against the stored
// hash. Returns true only on full match.
func (s *AuthStore) Verify(username, password string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if strings.EqualFold(u.Username, username) {
			return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
		}
	}
	// Constant-ish-time: still do a bcrypt compare against a dummy to
	// avoid leaking "user not found" via response time.
	_ = bcrypt.CompareHashAndPassword(
		[]byte("$2a$12$000000000000000000000000000000000000000000000000000000"),
		[]byte(password),
	)
	return false
}

// FirstUser returns the single admin's username (ProxyCTL is single-
// operator). Used for log lines + the optional ?user= URL pre-fill.
func (s *AuthStore) FirstUser() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.users) == 0 {
		return "", false
	}
	return s.users[0].Username, true
}

// HashPassword returns a bcrypt hash (cost 12, matching GameCTL).
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", err
	}
	return string(h), nil
}
