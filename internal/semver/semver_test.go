package semver

import "testing"

func TestIsValid(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		// Valid — basic.
		{"0.0.0", true},
		{"1.2.3", true},
		{"10.20.30", true},
		// Valid — prerelease.
		{"1.2.3-rc.1", true},
		{"0.1.0-alpha", true},
		{"1.0.0-0.3.7", true},
		// Valid — build metadata.
		{"1.0.0+build.7", true},
		{"1.0.0-rc.1+exp.sha.5114f85", true},
		// Invalid — short.
		{"1", false},
		{"1.2", false},
		// Invalid — leading v.
		{"v1.2.3", false},
		// Invalid — leading zeros.
		{"01.0.0", false},
		{"1.02.3", false},
		// Invalid — common typos.
		{"latest", false},
		{"main", false},
		{"", false},
		// Invalid — trailing garbage.
		{"1.2.3 ", false},
		{"1.2.3-", false},
	}
	for _, c := range cases {
		if got := IsValid(c.v); got != c.want {
			t.Errorf("IsValid(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}
