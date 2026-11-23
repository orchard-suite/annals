package store

import "testing"

// A token Annals issued under its former name keeps working after the
// rename: its hash covers the whole secret, the old prefix with it.
func TestIssuedPrefix(t *testing.T) {
	for secret, want := range map[string]bool{
		TokenPrefix + "abc":       true,
		formerTokenPrefix + "abc": true,
		"nope":                    false,
		"xx_abc":                  false,
		"":                        false,
	} {
		if got := issuedPrefix(secret); got != want {
			t.Errorf("issuedPrefix(%q) = %v, want %v", secret, got, want)
		}
	}
}
