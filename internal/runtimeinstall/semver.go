package runtimeinstall

import (
	"sort"
	"strconv"
	"strings"
)

// Minimal semver + npm range support, stdlib only.
//
// Tandem asks npm which versions exist, then decides for itself which of them
// are newest and which satisfy an agent's declared compatibility range. It
// deliberately does not ask npm to resolve the range, because `npm view
// <pkg>@<range> version` answers with whatever that range happens to select and
// its output shape varies with how many versions match — a single string for one
// match, an array for several. Reading "the newest" off the end of that output
// silently returns the wrong answer whenever a range matches exactly one
// version, which is precisely what `^0.0.x` does (npm reads a caret on a 0.0.x
// version as that patch alone).

type semver struct {
	major, minor, patch int
	// pre holds dot-separated prerelease identifiers; empty for a release.
	pre []string
	// build holds build metadata, which semver excludes from precedence. Tandem
	// uses it to mark fork distributions (`0.0.33+fork.<sha>`), so it is kept for
	// display and for breaking ties in a deterministic order.
	build string
	raw   string
}

func parseSemver(s string) (semver, bool) {
	v := semver{raw: s}
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	// Build metadata does not participate in precedence; keep it aside.
	if plus := strings.IndexByte(s, '+'); plus >= 0 {
		v.build, s = s[plus+1:], s[:plus]
	}
	core := s
	if dash := strings.IndexByte(s, '-'); dash >= 0 {
		core = s[:dash]
		if rest := s[dash+1:]; rest != "" {
			v.pre = strings.Split(rest, ".")
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	out := [3]int{}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return semver{}, false
		}
		out[i] = n
	}
	v.major, v.minor, v.patch = out[0], out[1], out[2]
	return v, true
}

// compare returns -1, 0 or 1 following semver precedence: numeric core first,
// then prerelease, where having a prerelease sorts *before* not having one.
func (v semver) compare(o semver) int {
	for _, pair := range [][2]int{{v.major, o.major}, {v.minor, o.minor}, {v.patch, o.patch}} {
		if pair[0] != pair[1] {
			return sign(pair[0] - pair[1])
		}
	}
	if len(v.pre) == 0 && len(o.pre) == 0 {
		return 0
	}
	if len(v.pre) == 0 {
		return 1
	}
	if len(o.pre) == 0 {
		return -1
	}
	for i := 0; i < len(v.pre) && i < len(o.pre); i++ {
		a, b := v.pre[i], o.pre[i]
		an, aNum := strconv.Atoi(a)
		bn, bNum := strconv.Atoi(b)
		switch {
		case aNum == nil && bNum == nil:
			if an != bn {
				return sign(an - bn)
			}
		case aNum == nil:
			// Numeric identifiers always have lower precedence than alphanumeric.
			return -1
		case bNum == nil:
			return 1
		default:
			if a != b {
				if a < b {
					return -1
				}
				return 1
			}
		}
	}
	return sign(len(v.pre) - len(o.pre))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// semverNewer reports whether candidate is a strictly newer version than
// current. Unparseable input is never "newer", so a malformed npm answer can
// only ever fail to offer an update, not offer a bogus one.
func semverNewer(candidate, current string) bool {
	a, aok := parseSemver(candidate)
	b, bok := parseSemver(current)
	if !aok || !bok {
		return false
	}
	return a.compare(b) > 0
}

// comparator is one bound of a range, e.g. ">=1.2.0".
type comparator struct {
	op string // one of <, <=, >, >=, =
	v  semver
}

func (c comparator) matches(v semver) bool {
	cmp := v.compare(c.v)
	switch c.op {
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	default:
		return cmp == 0
	}
}

// versionRange is a disjunction (`||`) of conjunctions (space-separated bounds),
// mirroring npm's range grammar.
type versionRange struct {
	groups [][]comparator
	// anyVersion is set for `*` or an empty range, which match everything.
	anyVersion bool
	// hasPrerelease records whether the range itself mentions a prerelease. npm
	// excludes prerelease versions from ordinary ranges, so `^1.0.0` must not
	// match `2.0.0-beta.1`; a range that names a prerelease opts back in.
	hasPrerelease bool
}

// parseRange understands the npm range syntax Tandem's pins actually use:
// exact versions, `^`, `~`, the comparison operators, `*`/`x` wildcards, and
// both composition forms. Hyphen ranges (`1.2.3 - 2.0.0`) are not supported and
// are reported as invalid rather than silently mis-parsed.
func parseRange(constraint string) (versionRange, bool) {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" || constraint == "*" || constraint == "x" {
		return versionRange{anyVersion: true}, true
	}
	if strings.Contains(constraint, " - ") {
		return versionRange{}, false
	}
	out := versionRange{hasPrerelease: strings.Contains(constraint, "-")}
	for _, alt := range strings.Split(constraint, "||") {
		alt = strings.TrimSpace(alt)
		if alt == "" || alt == "*" || alt == "x" {
			return versionRange{anyVersion: true}, true
		}
		var group []comparator
		for _, token := range strings.Fields(alt) {
			bounds, ok := parseComparators(token)
			if !ok {
				return versionRange{}, false
			}
			group = append(group, bounds...)
		}
		if len(group) == 0 {
			return versionRange{}, false
		}
		out.groups = append(out.groups, group)
	}
	if len(out.groups) == 0 {
		return versionRange{}, false
	}
	return out, true
}

func parseComparators(token string) ([]comparator, bool) {
	switch {
	case strings.HasPrefix(token, "^"):
		return caretComparators(token[1:])
	case strings.HasPrefix(token, "~"):
		return tildeComparators(strings.TrimPrefix(token[1:], ">"))
	case strings.HasPrefix(token, ">="):
		return simpleComparator(">=", token[2:])
	case strings.HasPrefix(token, "<="):
		return simpleComparator("<=", token[2:])
	case strings.HasPrefix(token, ">"):
		return simpleComparator(">", token[1:])
	case strings.HasPrefix(token, "<"):
		return simpleComparator("<", token[1:])
	case strings.HasPrefix(token, "="):
		return exactOrPartial(token[1:])
	default:
		return exactOrPartial(token)
	}
}

func simpleComparator(op, rest string) ([]comparator, bool) {
	v, _, ok := parsePartial(rest)
	if !ok {
		return nil, false
	}
	return []comparator{{op: op, v: v}}, true
}

// exactOrPartial turns a bare version into bounds. A partial version behaves as
// a range over the unspecified component: `1.2` means `>=1.2.0 <1.3.0`.
func exactOrPartial(rest string) ([]comparator, bool) {
	v, specified, ok := parsePartial(rest)
	if !ok {
		return nil, false
	}
	if specified == 3 {
		return []comparator{{op: "=", v: v}}, true
	}
	return []comparator{{op: ">=", v: v}, {op: "<", v: bumpAt(v, specified-1)}}, true
}

// caretComparators implements npm's caret: allow changes that do not modify the
// left-most non-zero component. ^1.2.3 -> >=1.2.3 <2.0.0, ^0.2.3 -> <0.3.0, and
// ^0.0.3 -> <0.0.4, which is why a caret on a 0.0.x version admits exactly one
// version.
func caretComparators(rest string) ([]comparator, bool) {
	v, specified, ok := parsePartial(rest)
	if !ok {
		return nil, false
	}
	upper := semver{}
	switch {
	case v.major != 0:
		upper = semver{major: v.major + 1}
	case v.minor != 0 || specified < 2:
		upper = semver{minor: v.minor + 1}
	case v.patch != 0 || specified < 3:
		upper = semver{minor: v.minor, patch: v.patch + 1}
	default:
		// ^0.0.0 admits only 0.0.0.
		upper = semver{patch: 1}
	}
	return []comparator{{op: ">=", v: v}, {op: "<", v: upper}}, true
}

// tildeComparators implements npm's tilde: allow patch-level changes when a
// minor is given, minor-level changes when it is not. ~0.0.3 -> >=0.0.3 <0.1.0,
// which is the range a 0.0.x package actually wants.
func tildeComparators(rest string) ([]comparator, bool) {
	v, specified, ok := parsePartial(rest)
	if !ok {
		return nil, false
	}
	upper := semver{major: v.major, minor: v.minor + 1}
	if specified == 1 {
		upper = semver{major: v.major + 1}
	}
	return []comparator{{op: ">=", v: v}, {op: "<", v: upper}}, true
}

// parsePartial accepts a full or partial version, returning it zero-filled plus
// how many components were actually given.
func parsePartial(s string) (semver, int, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "*" || s == "x" {
		return semver{}, 0, false
	}
	pre := ""
	if dash := strings.IndexByte(s, '-'); dash >= 0 {
		pre, s = s[dash+1:], s[:dash]
	}
	if plus := strings.IndexByte(s, '+'); plus >= 0 {
		s = s[:plus]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return semver{}, 0, false
	}
	v := semver{raw: s}
	specified := 0
	for i, part := range parts {
		if part == "x" || part == "X" || part == "*" {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return semver{}, 0, false
		}
		switch i {
		case 0:
			v.major = n
		case 1:
			v.minor = n
		case 2:
			v.patch = n
		}
		specified++
	}
	if specified == 0 {
		return semver{}, 0, false
	}
	if pre != "" {
		v.pre = strings.Split(pre, ".")
	}
	return v, specified, true
}

// bumpAt increments component idx (0=major, 1=minor) and zeroes the rest.
func bumpAt(v semver, idx int) semver {
	switch idx {
	case 0:
		return semver{major: v.major + 1}
	case 1:
		return semver{major: v.major, minor: v.minor + 1}
	default:
		return semver{major: v.major, minor: v.minor, patch: v.patch + 1}
	}
}

func (r versionRange) matches(v semver) bool {
	if r.anyVersion {
		return true
	}
	if len(v.pre) > 0 && !r.hasPrerelease {
		return false
	}
	for _, group := range r.groups {
		ok := true
		for _, c := range group {
			if !c.matches(v) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// satisfies reports whether version is inside constraint. An unparseable
// constraint is treated as matching nothing, so a typo cannot silently widen
// what Tandem considers compatible.
func satisfies(version, constraint string) bool {
	v, ok := parseSemver(version)
	if !ok {
		return false
	}
	r, ok := parseRange(constraint)
	if !ok {
		return false
	}
	return r.matches(v)
}

// newestInRange returns the highest version in versions that satisfies
// constraint.
func newestInRange(versions []string, constraint string) (string, bool) {
	r, ok := parseRange(constraint)
	if !ok {
		return "", false
	}
	best, found := semver{}, false
	for _, raw := range versions {
		v, ok := parseSemver(raw)
		if !ok || !r.matches(v) {
			continue
		}
		if !found || v.compare(best) > 0 {
			best, found = v, true
		}
	}
	return best.raw, found
}

// newestPublished returns the highest release in versions, ignoring
// prereleases: an update offer should never move an operator onto a beta
// because it happens to sort highest.
func newestPublished(versions []string) (string, bool) {
	best, found := semver{}, false
	for _, raw := range versions {
		v, ok := parseSemver(raw)
		if !ok || len(v.pre) > 0 {
			continue
		}
		if !found || v.compare(best) > 0 {
			best, found = v, true
		}
	}
	return best.raw, found
}

// sortVersions orders versions newest first. Unparseable entries sort last in
// input order. Semver gives build metadata no precedence, so a plain release and
// a fork build of it (`0.0.33` vs `0.0.33+fork.<sha>`) compare equal; the plain
// release is listed first so the order is stable and reads sensibly.
func sortVersions(values []string) {
	sort.SliceStable(values, func(i, j int) bool {
		a, aok := parseSemver(values[i])
		b, bok := parseSemver(values[j])
		if aok != bok {
			return aok
		}
		if !aok {
			return false
		}
		if cmp := a.compare(b); cmp != 0 {
			return cmp > 0
		}
		return a.build == "" && b.build != ""
	})
}
