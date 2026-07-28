package main

import (
	"strings"
	"testing"
)

// The regression that motivated this: an apex web route ("examplelabs.cc")
// was zone-resolved by stripping one label, producing a lookup for zone
// "cc" — the fqdn itself must be the FIRST candidate. Multi-level
// subdomains broke the same way (only one label was ever stripped).
func TestZoneCandidates(t *testing.T) {
	cases := map[string][]string{
		"examplelabs.cc":        {"examplelabs.cc"},
		"app.examplelabs.cc":    {"app.examplelabs.cc", "examplelabs.cc"},
		"a.b.examplelabs.cc":    {"a.b.examplelabs.cc", "b.examplelabs.cc", "examplelabs.cc"},
		"examplelabs.CC":        {"examplelabs.cc"}, // normalized
		"localhost":           nil,              // not an FQDN
		"":                    nil,
	}
	for in, want := range cases {
		got := zoneCandidates(in)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("zoneCandidates(%q) = %v, want %v", in, got, want)
		}
	}
}
