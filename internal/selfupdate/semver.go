package selfupdate

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

type semver struct {
	core [3]uint64 // major, minor, patch
	pre  []string  // prerelease identifiers; nil for a release
}

// CompareSemver compares two semantic versions and returns -1, 0 or 1. Each
// may have a leading "v". MAJOR.MINOR.PATCH compare numerically, a
// prerelease ranks below its release, prerelease identifiers compare per
// semver §11, and +build metadata is ignored. Anything that is not a strict
// semantic version (such as "dev") is an error.
func CompareSemver(a, b string) (int, error) {
	va, err := parseSemver(a)
	if err != nil {
		return 0, err
	}
	vb, err := parseSemver(b)
	if err != nil {
		return 0, err
	}
	for i := range va.core {
		if c := cmp.Compare(va.core[i], vb.core[i]); c != 0 {
			return c, nil
		}
	}
	return comparePrerelease(va.pre, vb.pre), nil
}

func parseSemver(s string) (semver, error) {
	invalid := fmt.Errorf("invalid semantic version %q", s)
	v := strings.TrimPrefix(s, "v")

	if rest, build, ok := strings.Cut(v, "+"); ok {
		if !validIdentifiers(build, false) {
			return semver{}, invalid
		}
		v = rest
	}
	core, pre, hasPre := strings.Cut(v, "-")

	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, invalid
	}
	var out semver
	for i, p := range parts {
		if !isNumeric(p) || (len(p) > 1 && p[0] == '0') {
			return semver{}, invalid
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return semver{}, invalid
		}
		out.core[i] = n
	}
	if hasPre {
		if !validIdentifiers(pre, true) {
			return semver{}, invalid
		}
		out.pre = strings.Split(pre, ".")
	}
	return out, nil
}

// validIdentifiers reports whether s is a dot-separated list of non-empty
// [0-9A-Za-z-] identifiers. Prerelease numeric identifiers must not have
// leading zeros.
func validIdentifiers(s string, prerelease bool) bool {
	for id := range strings.SplitSeq(s, ".") {
		if id == "" {
			return false
		}
		for _, c := range []byte(id) {
			if !isIdentifierChar(c) {
				return false
			}
		}
		if prerelease && isNumeric(id) && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

func isIdentifierChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-'
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func comparePrerelease(a, b []string) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1 // a release ranks above any prerelease
	case b == nil:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareIdentifier(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

// compareIdentifier compares numeric identifiers numerically (by length, then
// digits, so any size works), ranks numeric below alphanumeric, and compares
// alphanumeric identifiers in ASCII order.
func compareIdentifier(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		if c := cmp.Compare(len(a), len(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}
