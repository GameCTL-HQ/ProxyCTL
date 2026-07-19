package main

// Embedded, structured changelog — the ProxyCTL counterpart to GameCTL's
// internal/releasenotes. The source of truth is releases.json (the human
// companion is the GitHub Releases page — keep them in sync). It is compiled
// into the binary via //go:embed so the running build always carries its own
// notes; the in-app "What's new" panel surfaces them via GET /api/release-notes.
//
// Version alignment: builds are stamped with -X main.version=<tag-or-sha>. We
// match the running version against a tagged entry; if none matches (the common
// case for SHA/dev builds) we fall back to the "unreleased" entry so the
// operator still sees what changed.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
)

//go:embed releases.json
var releaseNotesRaw []byte

// relChange is a single documented fix/addition within a release.
type relChange struct {
	Type   string `json:"type"`   // fixed | added | changed | removed | security
	Title  string `json:"title"`  // short "what's updating"
	Detail string `json:"detail"` // "and why"
}

// relEntry is one changelog entry, aligned to a build version.
type relEntry struct {
	Version    string      `json:"version"` // git tag or "unreleased"
	Name       string      `json:"name,omitempty"`
	Unreleased bool        `json:"unreleased,omitempty"`
	Date       string      `json:"date,omitempty"`
	Summary    string      `json:"summary,omitempty"`
	Changes    []relChange `json:"changes"`
}

type relDoc struct {
	Releases []relEntry `json:"releases"`
}

var relParsed relDoc

// relLoadErr is non-nil only if the embedded JSON is malformed (a build-time
// authoring mistake). It is surfaced so a bad changelog is caught in CI / at
// startup rather than silently serving nothing.
var relLoadErr error

func init() {
	relLoadErr = json.Unmarshal(releaseNotesRaw, &relParsed)
}

// relAll returns every release entry, newest first (file order).
func relAll() ([]relEntry, error) {
	if relLoadErr != nil {
		return nil, fmt.Errorf("release notes: %w", relLoadErr)
	}
	return relParsed.Releases, nil
}

// relForVersion returns the release entry that best matches the running build:
//
//   - an exact version match (tag-stamped releases), else
//   - the "unreleased" entry (SHA/dev builds that aren't tagged yet), else
//   - the newest entry as a last resort.
//
// found is false only when there are no entries at all.
func relForVersion(v string) (rel relEntry, found bool, err error) {
	all, err := relAll()
	if err != nil {
		return relEntry{}, false, err
	}
	if len(all) == 0 {
		return relEntry{}, false, nil
	}

	n := normVer(v)
	var unreleased *relEntry
	for i := range all {
		r := &all[i]
		if n != "" && n != "dev" && normVer(r.Version) == n {
			return *r, true, nil
		}
		if unreleased == nil && (r.Unreleased || normVer(r.Version) == "unreleased") {
			unreleased = r
		}
	}
	if unreleased != nil {
		return *unreleased, true, nil
	}
	return all[0], true, nil
}

// relIsTagged reports whether the running build's version exactly matches a
// tagged (non-unreleased) entry — i.e. this is a released build. Released
// builds hide the "unreleased" staging entry from the in-app notes: it only
// carries meaning on SHA/dev builds, and on a release it's either a
// duplicate of the release itself or an empty placeholder.
func relIsTagged(v string) bool {
	all, err := relAll()
	if err != nil {
		return false
	}
	n := normVer(v)
	if n == "" || n == "dev" {
		return false
	}
	for i := range all {
		r := &all[i]
		if !r.Unreleased && normVer(r.Version) != "unreleased" && normVer(r.Version) == n {
			return true
		}
	}
	return false
}

// releaseNotes serves ProxyCTL's embedded changelog so the in-app "What's new"
// panel can show "what's updating and why". It returns every entry plus the one
// matching the running build (exact tag match, else the "unreleased" entry for
// SHA/dev builds) so the UI can highlight the relevant version in one round-trip.
// Released (tag-stamped) builds don't list the "unreleased" staging entry —
// that's dev-build territory.
func (a *API) releaseNotes(w http.ResponseWriter, _ *http.Request) {
	all, err := relAll()
	if err != nil {
		writeJSONResp(w, http.StatusInternalServerError, map[string]any{
			"error": "release notes unavailable: " + err.Error(),
		})
		return
	}
	if relIsTagged(version) {
		kept := make([]relEntry, 0, len(all))
		for _, r := range all {
			if r.Unreleased || normVer(r.Version) == "unreleased" {
				continue
			}
			kept = append(kept, r)
		}
		all = kept
	}
	resp := map[string]any{
		"version":  version,
		"releases": all,
	}
	if current, found, _ := relForVersion(version); found {
		resp["current"] = current
	}
	writeJSONResp(w, http.StatusOK, resp)
}
