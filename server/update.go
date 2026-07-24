package main

// In-app update notify + one-click self-update — the ProxyCTL counterpart to
// GameCTL's internal/update + /api/update/{check,apply}. We poll the public
// GitHub repo's latest release, compare it to the running build, and surface
// "update available" to the UI; "apply" moves ProxyCTL's own Deployment to that
// release by pinning the container image to its immutable tag. Pinning is the
// point: because the deployed image is a fixed version tag, an ordinary pod
// restart (node drain, eviction, OOM, re-running the installer) re-pulls the
// SAME version instead of silently jumping to whatever a moving :latest tag now
// points at. Only this explicit action changes the version. The auth Secret,
// droplet config, and every gateway are untouched, so no re-setup is needed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// version is stamped at build time via `-ldflags "-X main.version=<sha-or-tag>"`
// (see Dockerfile / clusterdeploy.sh). "dev" for un-stamped local builds.
var version = "dev"

// updateRepo is the public GitHub repo whose releases drive the update check
// (the same repo the proxyctl.cc site polls).
const updateRepo = "GameCTL-HQ/ProxyCTL"

// updateStatus is the wire shape returned to the UI.
type updateStatus struct {
	Current         string    `json:"current"`
	Latest          string    `json:"latest"`
	UpdateAvailable bool      `json:"updateAvailable"`
	ReleaseURL      string    `json:"releaseUrl,omitempty"`
	CheckedAt       time.Time `json:"checkedAt"`
	Note            string    `json:"note,omitempty"`
}

// updateChecker polls a GitHub repo's latest release, with a TTL cache so the
// API isn't hit on every page load.
type updateChecker struct {
	repo    string
	current string
	httpc   *http.Client
	ttl     time.Duration

	mu     sync.Mutex
	cache  updateStatus
	cached time.Time
	ok     bool
}

func newUpdateChecker(repo, current string) *updateChecker {
	return &updateChecker{
		repo:    repo,
		current: current,
		httpc:   &http.Client{Timeout: 6 * time.Second},
		ttl:     30 * time.Minute,
	}
}

// normVer strips a leading "v" so "v0.0.1-beta" == "0.0.1-beta".
func normVer(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "v") }

// parseSemver parses "X.Y.Z" / "X.Y.Z-suffix". ok=false for anything else
// (commit SHAs, "dev") so we decline to compare instead of guessing.
func parseSemver(s string) (nums [3]int, pre string, ok bool) {
	s = normVer(s)
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	seg := strings.Split(s, ".")
	if len(seg) != 3 {
		return nums, pre, false
	}
	for i, t := range seg {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			return nums, pre, false
		}
		nums[i] = n
	}
	return nums, pre, true
}

// versionLess reports a < b. Unparseable inputs → false, so a SHA-stamped
// homelab build or a transient GitHub lag never falsely flags an update.
// A prerelease (-beta) sorts below the same X.Y.Z without one (SemVer §11.4.3).
func versionLess(a, b string) bool {
	pa, prea, oka := parseSemver(a)
	pb, preb, okb := parseSemver(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	switch {
	case prea == preb:
		return false
	case prea == "" && preb != "":
		return false
	case prea != "" && preb == "":
		return true
	default:
		return prea < preb
	}
}

// check returns the cached status if fresh, else queries GitHub. Never errors:
// transient problems surface as Status.Note with UpdateAvailable=false.
func (c *updateChecker) check(ctx context.Context, force bool) updateStatus {
	c.mu.Lock()
	if !force && c.ok && time.Since(c.cached) < c.ttl {
		s := c.cache
		c.mu.Unlock()
		return s
	}
	c.mu.Unlock()

	s := updateStatus{Current: c.current, CheckedAt: time.Now().UTC()}
	latest, url, err := c.fetchLatest(ctx)
	if err != nil {
		s.Note = "could not check for updates"
		c.mu.Lock()
		c.cache, c.cached, c.ok = s, time.Now().Add(-c.ttl+5*time.Minute), true
		c.mu.Unlock()
		return s
	}
	s.Latest = latest
	s.ReleaseURL = url
	if c.current != "" && c.current != "dev" && latest != "" && versionLess(c.current, latest) {
		s.UpdateAvailable = true
	}
	c.mu.Lock()
	c.cache, c.cached, c.ok = s, time.Now(), true
	c.mu.Unlock()
	return s
}

func (c *updateChecker) fetchLatest(ctx context.Context) (tag, htmlURL string, err error) {
	if c.repo == "" {
		return "", "", errors.New("no update repo configured")
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	code, err := c.getJSON(ctx, fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", c.repo), &rel)
	if err == nil && code == http.StatusOK && rel.TagName != "" {
		return rel.TagName, rel.HTMLURL, nil
	}
	var tags []struct {
		Name string `json:"name"`
	}
	code, err = c.getJSON(ctx, fmt.Sprintf("https://api.github.com/repos/%s/tags?per_page=1", c.repo), &tags)
	if err != nil {
		return "", "", err
	}
	if code != http.StatusOK {
		return "", "", fmt.Errorf("github api: HTTP %d", code)
	}
	if len(tags) == 0 || tags[0].Name == "" {
		return "", "", errors.New("no releases or tags found")
	}
	return tags[0].Name, fmt.Sprintf("https://github.com/%s/releases/tag/%s", c.repo, tags[0].Name), nil
}

func (c *updateChecker) getJSON(ctx context.Context, url string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "proxyctl-update-check")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// --- HTTP handlers ---

func writeJSONResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// version reports the running build (stamped at release; "dev" otherwise).
func (a *API) version(w http.ResponseWriter, _ *http.Request) {
	writeJSONResp(w, http.StatusOK, map[string]any{"version": version})
}

// updateCheck returns cached "is a newer release available?" info. Never errors.
func (a *API) updateCheck(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	writeJSONResp(w, http.StatusOK, a.upd.check(r.Context(), force))
}

// splitImageRepoTag splits a container image reference into its repository and
// tag, tolerating a registry host:port (a colon that precedes the last '/') and
// an optional @digest. Returns tag "" when the ref carries no tag.
func splitImageRepoTag(image string) (repo, tag string) {
	if at := strings.LastIndex(image, "@"); at >= 0 {
		image = image[:at] // drop any @sha256:… digest
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		return image[:colon], image[colon+1:]
	}
	return image, ""
}

// selfContainerImage reads the running Deployment's first container name and
// image via kubectl, so updateApply can keep the current registry/repo and swap
// only the tag.
func (a *API) selfContainerImage(ctx context.Context) (name, image string, err error) {
	out, err := exec.CommandContext(ctx, a.kubectl, "-n", a.selfNS,
		"get", "deploy/"+a.selfDeploy,
		"-o", "jsonpath={.spec.template.spec.containers[0].name} {.spec.template.spec.containers[0].image}").CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return "", "", fmt.Errorf("unexpected deployment image output: %q", strings.TrimSpace(string(out)))
	}
	return f[0], f[1], nil
}

// updateApply moves ProxyCTL's own Deployment to the newest published release by
// setting the container image to that immutable tag (keeping the current
// registry/repo). Only this explicit action changes the version — because the
// deployed image is pinned to a fixed tag, an ordinary pod restart re-pulls the
// SAME version rather than silently jumping to whatever a moving :latest tag now
// points at. Uses the in-cluster kubectl + ProxyCTL's ServiceAccount, which the
// proxyctl-gateway-mgr Role already allows to get+patch deployments in-namespace.
func (a *API) updateApply(w http.ResponseWriter, r *http.Request) {
	st := a.upd.check(r.Context(), true)
	latest := strings.TrimSpace(st.Latest)
	if latest == "" {
		writeJSONResp(w, http.StatusBadGateway, map[string]any{
			"error": "couldn't determine the latest release to update to — check connectivity to GitHub and try again.",
		})
		return
	}
	name, curImage, err := a.selfContainerImage(r.Context())
	if err != nil {
		writeJSONResp(w, http.StatusInternalServerError, map[string]any{
			"error": "couldn't read the current image: " + err.Error(),
		})
		return
	}
	repo, curTag := splitImageRepoTag(curImage)
	if curTag == latest {
		writeJSONResp(w, http.StatusOK, map[string]any{
			"ok":         true,
			"deployment": a.selfNS + "/" + a.selfDeploy,
			"message":    "Already running " + latest + " — nothing to update.",
		})
		return
	}
	newImage := repo + ":" + latest
	result, err := a.applyRelease(r.Context(), name, newImage, curTag, latest)
	if err != nil {
		writeJSONResp(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	resp := map[string]any{
		"ok":         true,
		"deployment": a.selfNS + "/" + a.selfDeploy,
		"from":       curTag,
		"to":         latest,
		"message":    result.message,
	}
	if result.rbacWarning != "" {
		resp["warning"] = result.rbacWarning
	}
	writeJSONResp(w, http.StatusOK, resp)
}
