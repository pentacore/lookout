package selfupdate

import "testing"

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.2.0", -1},
		{"0.10.0", "0.9.0", 1},
		{"v1.0.0", "1.0.0", 0},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.2.3", "1.2.4", -1},
		{"1.0.0+build.5", "1.0.0", 0},
		{"1.0.0-rc.1+build.5", "1.0.0-rc.1+build.6", 0},
		// semver §11 precedence chain.
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-alpha.beta", "1.0.0-beta", -1},
		{"1.0.0-beta", "1.0.0-beta.2", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-beta.11", "1.0.0-rc.1", -1},
		{"1.0.0-1", "1.0.0-alpha", -1},
		{"1.0.0-rc-1", "1.0.0-rc-1", 0},
		{"1.0.0-99999999999999999999", "1.0.0-100000000000000000000", -1},
		{"0.0.0-SNAPSHOT-abc1234", "0.1.0", -1},
	}
	for _, tt := range tests {
		got, err := CompareSemver(tt.a, tt.b)
		if err != nil {
			t.Errorf("CompareSemver(%q, %q): %v", tt.a, tt.b, err)
			continue
		}
		if got != tt.want {
			t.Errorf("CompareSemver(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCompareSemverRejectsInvalid(t *testing.T) {
	for _, bad := range []string{
		"dev", "", "v", "1", "1.0", "1.0.0.0", "01.0.0", "1.00.0", "1.0.-1",
		"1.0.0-", "1.0.0-rc..1", "1.0.0-01", "1.0.0-rc_1", "1.0.0+", "1.0.0+a..b",
		"1.0.0 ", " 1.0.0", "1.0.0\n", "vv1.0.0", "1.0.0-rc.1\nlookout", "１.0.0",
		"99999999999999999999.0.0",
	} {
		if _, err := CompareSemver(bad, "1.0.0"); err == nil {
			t.Errorf("CompareSemver(%q, 1.0.0) accepted an invalid version", bad)
		}
		if _, err := CompareSemver("1.0.0", bad); err == nil {
			t.Errorf("CompareSemver(1.0.0, %q) accepted an invalid version", bad)
		}
	}
}
