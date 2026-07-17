package main

import "testing"

// TestEmbeddedReleasesValid catches an authoring mistake in releases.json at
// CI/deploy time rather than letting the API silently serve nothing.
func TestEmbeddedReleasesValid(t *testing.T) {
	if relLoadErr != nil {
		t.Fatalf("releases.json failed to parse: %v", relLoadErr)
	}
	all, err := relAll()
	if err != nil {
		t.Fatalf("relAll: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no release entries embedded")
	}
	for i, r := range all {
		if r.Version == "" {
			t.Errorf("release %d has empty version", i)
		}
		// The "unreleased" placeholder is intentionally empty between releases
		// (we don't list uncommitted/in-flight work at the top of What's new);
		// every *tagged* entry must carry changes.
		if len(r.Changes) == 0 && !r.Unreleased && normVer(r.Version) != "unreleased" {
			t.Errorf("tagged release %q has no changes", r.Version)
		}
		for j, c := range r.Changes {
			if c.Title == "" || c.Type == "" {
				t.Errorf("release %q change %d missing type/title", r.Version, j)
			}
		}
	}
}

func TestRelForVersion(t *testing.T) {
	// Untagged SHA build falls back to the "unreleased" entry.
	r, found, err := relForVersion("874e7af")
	if err != nil || !found {
		t.Fatalf("relForVersion SHA: found=%v err=%v", found, err)
	}
	if !r.Unreleased && normVer(r.Version) != "unreleased" {
		t.Errorf("SHA build should resolve to the unreleased entry, got %q", r.Version)
	}

	// "dev" must never panic and should still resolve to something.
	if _, found, _ := relForVersion("dev"); !found {
		t.Error("dev build should still resolve an entry")
	}

	// An exact tag match resolves to that entry (with or without a leading v).
	if r, found, _ := relForVersion("v0.2.2"); !found || normVer(r.Version) != "0.2.2" {
		t.Errorf("v0.2.2 should resolve to the 0.2.2 entry, got found=%v ver=%q", found, r.Version)
	}
}
