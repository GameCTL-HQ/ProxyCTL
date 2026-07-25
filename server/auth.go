package main

// JWT-based browser + API auth, matching GameCTL exactly:
//
//   - Storage: Kubernetes Secret (`proxyctl-auth`) holding `jwt` (signing
//     key) and `users.json` (bcrypt records). See [authstore.go].
//   - Setup mode: triggered when the Secret is missing or empty. An
//     ephemeral random JWT key + a one-time bootstrap token are generated
//     in memory; the token is logged once (slog JSON, scrape-friendly).
//   - Claim: POST /api/auth/setup {bootstrapToken, username, password}.
//     On success, the JWT key + bcrypt record are persisted into the
//     Secret, the in-memory authenticator is "adopted" (no restart
//     needed), and an `access_token` is returned in the JSON response so
//     the operator is signed in immediately.
//   - Login: POST /api/token {username, password} → {access_token,
//     token_type:"bearer"}. JWT is HS256, 8-hour exp, sub=username.
//   - Auth middleware: `Authorization: Bearer <JWT>` on every protected
//     /api/* route. No cookies. No HTTP Basic. JWT in localStorage on the
//     UI side, same as GameCTL.
//   - Recovery: `kubectl delete secret proxyctl-auth` + rollout restart →
//     fresh bootstrap token on the next pod log.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const minPasswordLen = 12

// addrIsLoopback is kept (still used by main.go's public-bind safety check).
func addrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authn is the JWT authenticator. Built once in main and threaded through
// the API. The JWT signing key lives behind a RWMutex so the post-claim
// "adopt" step can swap in the persisted key without restarting.
type authn struct {
	store *AuthStore

	mu        sync.RWMutex
	jwtKey    []byte
	setupMode bool
	bootstrap string // empty once consumed or never set
	disabled  bool   // dev-only: skip auth entirely (loopback-gated in main)
}

// newAuthn loads the Secret and decides between "ready" and "setup mode".
// In setup mode it generates an ephemeral JWT key (so the just-claimed
// admin is logged in immediately after setup; the key is replaced with
// the persisted one on adopt, so tokens issued post-setup outlive the
// process) and a one-time 32-hex-char bootstrap token.
func newAuthn(store *AuthStore, disabled bool) (*authn, error) {
	a := &authn{store: store, disabled: disabled}
	if disabled {
		slog.Warn("auth: DISABLED — dev mode only (loopback-gated)")
		return a, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, err := store.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth load: %w", err)
	}
	if !ok {
		eph, err := genEphemeralKey()
		if err != nil {
			return nil, fmt.Errorf("auth: generate ephemeral JWT key: %w", err)
		}
		tok, err := genBootstrap()
		if err != nil {
			return nil, fmt.Errorf("auth: generate bootstrap token: %w", err)
		}
		a.jwtKey = eph
		a.bootstrap = tok
		a.setupMode = true
		// Same JSON shape as GameCTL's slog.Warn — install.sh / clusterdeploy
		// extract this with the identical `"token":"…"` grep.
		slog.Warn("BOOTSTRAP TOKEN", "token", tok)
		slog.Info("auth: setup mode (no admin claimed yet) — paste the bootstrap token on the claim page.")
		return a, nil
	}
	a.jwtKey = store.JWTSecret()
	user, _ := store.FirstUser()
	slog.Info("auth: ready", "admin", user)
	return a, nil
}

// genEphemeralKey returns 48 random bytes base64-encoded (≥32 bytes of
// keying material, same shape as GameCTL's ephemeral setup key).
func genEphemeralKey() ([]byte, error) {
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return []byte(base64.StdEncoding.EncodeToString(raw)), nil
}

// genBootstrap returns a 32-hex-char random token (16 bytes / 128 bits).
func genBootstrap() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// NeedsSetup reports whether first-run setup is required.
func (a *authn) NeedsSetup() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.setupMode
}

// matchBootstrap constant-time compares the supplied token. False unless
// setup mode is active and a token has been issued.
func (a *authn) matchBootstrap(tok string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.setupMode || a.bootstrap == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(a.bootstrap)) == 1
}

// issueToken signs a JWT for the username. HS256, 8h expiry, sub=username
// — identical to GameCTL's IssueToken shape so the verification logic is
// the same on both apps.
func (a *authn) issueToken(username string) (string, error) {
	a.mu.RLock()
	secret := a.jwtKey
	a.mu.RUnlock()
	if len(secret) == 0 {
		return "", errors.New("no signing key")
	}
	claims := jwt.MapClaims{
		"sub": username,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(8 * time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(secret)
}

// parseToken validates the signed JWT and returns the subject.
func (a *authn) parseToken(s string) (string, error) {
	a.mu.RLock()
	secret := a.jwtKey
	a.mu.RUnlock()
	tok, err := jwt.Parse(s, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return secret, nil
	})
	if err != nil {
		return "", err
	}
	if !tok.Valid {
		return "", errors.New("invalid token")
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("invalid claims")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", errors.New("token has no subject")
	}
	return sub, nil
}

// middleware enforces a valid Bearer JWT on the request. Public auth
// endpoints (login / claim / state) bypass this; everything else /api/*
// requires it.
func (a *authn) middleware(next http.Handler) http.Handler {
	if a.disabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeAuthJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if _, err := a.parseToken(strings.TrimPrefix(h, "Bearer ")); err != nil {
			writeAuthJSON(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// state is public — the UI hits it on every load to decide between the
// claim form, the login form, and the main app.
func (a *authn) state(w http.ResponseWriter, _ *http.Request) {
	user, claimed := a.store.FirstUser()
	out := map[string]any{
		"needsSetup": a.NeedsSetup(),
		"claimed":    claimed,
	}
	if claimed {
		out["username"] = user
	}
	writeAuthJSONRaw(w, http.StatusOK, out)
}

type setupReq struct {
	BootstrapToken string `json:"bootstrapToken"`
	Username       string `json:"username"`
	Password       string `json:"password"`
}

// setup completes first-run admin provisioning. Public (pre-auth), gated
// by the one-time bootstrap token. Persists JWT key + bcrypt user into
// the Secret, then adopts in-memory, then issues a JWT — so the operator
// is signed in immediately with no restart.
func (a *authn) setup(w http.ResponseWriter, r *http.Request) {
	if !a.NeedsSetup() {
		writeAuthJSON(w, http.StatusConflict, "setup already completed")
		return
	}
	var req setupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !a.matchBootstrap(strings.TrimSpace(req.BootstrapToken)) {
		writeAuthJSON(w, http.StatusUnauthorized, "invalid bootstrap token")
		return
	}
	if len(req.Username) < 3 {
		writeAuthJSON(w, http.StatusBadRequest, "username must be at least 3 characters")
		return
	}
	if len(req.Password) < minPasswordLen {
		writeAuthJSON(w, http.StatusBadRequest, fmt.Sprintf("password must be at least %d characters", minPasswordLen))
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		writeAuthJSON(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	a.mu.RLock()
	jwtKey := a.jwtKey
	a.mu.RUnlock()

	// Persist BEFORE adopting in-memory so a write failure leaves the
	// server still in setup mode (retryable) rather than a state where
	// the process accepts logins that won't survive a restart.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.store.Save(ctx, jwtKey, []AuthUser{{Username: req.Username, PasswordHash: hash}}); err != nil {
		writeAuthJSON(w, http.StatusInternalServerError, "failed to persist credentials: "+err.Error())
		return
	}

	a.mu.Lock()
	a.setupMode = false
	a.bootstrap = ""
	a.mu.Unlock()
	slog.Info("auth: admin claimed — bootstrap token consumed", "user", req.Username)

	tok, err := a.issueToken(req.Username)
	if err != nil {
		// Credentials are persisted; operator can just log in normally.
		writeAuthJSONRaw(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeAuthJSONRaw(w, http.StatusOK, map[string]any{
		"ok":           true,
		"access_token": tok,
		"token_type":   "bearer",
	})
}

type tokenReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// tokenHandler is the JWT login endpoint (POST /api/token). JSON or
// form-encoded, same shape as GameCTL.
func (a *authn) tokenHandler(w http.ResponseWriter, r *http.Request) {
	if a.NeedsSetup() {
		writeAuthJSON(w, http.StatusServiceUnavailable, "setup not completed")
		return
	}
	var req tokenReq
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAuthJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			writeAuthJSON(w, http.StatusBadRequest, "invalid form")
			return
		}
		req.Username = r.PostFormValue("username")
		req.Password = r.PostFormValue("password")
	}
	if !a.store.Verify(req.Username, req.Password) {
		writeAuthJSON(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	tok, err := a.issueToken(req.Username)
	if err != nil {
		writeAuthJSON(w, http.StatusInternalServerError, "token issue failed")
		return
	}
	writeAuthJSONRaw(w, http.StatusOK, map[string]any{
		"access_token": tok,
		"token_type":   "bearer",
	})
}

// writeAuthJSON / writeAuthJSONRaw — local helpers so the auth package
// doesn't depend on the API's writeJSON method receiver. Same wire shape
// as the rest of the API (Content-Type: application/json).
func writeAuthJSON(w http.ResponseWriter, code int, errMsg string) {
	writeAuthJSONRaw(w, code, map[string]string{"error": errMsg})
}
func writeAuthJSONRaw(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
