package runtimeinstall

import (
	"slices"
	"strings"
	"testing"
)

func TestSemverNewer(t *testing.T) {
	for _, tc := range []struct {
		candidate, current string
		want               bool
	}{
		{"1.12.0", "1.8.0", true},
		{"1.8.0", "1.12.0", false},
		{"1.8.0", "1.8.0", false},
		{"0.0.33", "0.0.31", true},
		{"0.1.0", "0.0.99", true},
		{"2.0.0", "1.99.99", true},
		// A release outranks its own prereleases; a prerelease does not outrank
		// the release it leads to.
		{"1.8.0", "1.8.0-beta.1", true},
		{"1.8.0-beta.2", "1.8.0", false},
		{"1.8.0-beta.2", "1.8.0-beta.1", true},
		{"1.8.0-beta.10", "1.8.0-beta.2", true},
		{"1.8.0-alpha", "1.8.0-alpha.1", false},
		// Build metadata is not part of precedence.
		{"1.8.0+build.2", "1.8.0+build.1", false},
		// Unparseable input must never read as an upgrade.
		{"not-a-version", "1.0.0", false},
		{"1.0.0", "garbage", false},
		{"1.0", "0.9.0", false},
		{"0.0.33+fork.abc123", "0.0.33", false},
	} {
		if got := semverNewer(tc.candidate, tc.current); got != tc.want {
			t.Errorf("semverNewer(%q,%q)=%v want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

func TestSatisfiesCaretOnZeroZeroVersionsIsASingleVersion(t *testing.T) {
	// The bug this whole module exists for: npm reads a caret on a 0.0.x version
	// as that patch alone, so ^0.0.31 admits 0.0.31 and nothing else. Tandem's
	// pi pin was written as ^0.0.31 and its update check was therefore inert.
	if !satisfies("0.0.31", "^0.0.31") {
		t.Error("^0.0.31 must admit 0.0.31")
	}
	for _, v := range []string{"0.0.32", "0.0.33", "0.1.0", "0.0.30"} {
		if satisfies(v, "^0.0.31") {
			t.Errorf("^0.0.31 must not admit %s", v)
		}
	}
	// Tilde is the range a 0.0.x package actually wants.
	for _, v := range []string{"0.0.33", "0.0.34", "0.0.99"} {
		if !satisfies(v, "~0.0.33") {
			t.Errorf("~0.0.33 must admit %s", v)
		}
	}
	for _, v := range []string{"0.0.32", "0.1.0", "1.0.0"} {
		if satisfies(v, "~0.0.33") {
			t.Errorf("~0.0.33 must not admit %s", v)
		}
	}
}

func TestSatisfies(t *testing.T) {
	for _, tc := range []struct {
		version, constraint string
		want                bool
	}{
		// Caret keeps the left-most non-zero component.
		{"1.8.0", "^1.8.0", true},
		{"1.12.0", "^1.8.0", true},
		{"1.99.99", "^1.8.0", true},
		{"2.0.0", "^1.8.0", false},
		{"1.7.9", "^1.8.0", false},
		{"0.70.1", "^0.70.0", true},
		{"0.71.0", "^0.70.0", false},
		// Tilde allows patch moves when a minor is given.
		{"1.8.9", "~1.8.0", true},
		{"1.9.0", "~1.8.0", false},
		// Exact and operators.
		{"1.2.3", "1.2.3", true},
		{"1.2.4", "1.2.3", false},
		{"1.2.4", ">=1.2.3", true},
		{"1.2.3", ">1.2.3", false},
		{"1.2.2", "<1.2.3", true},
		{"1.2.3", "<=1.2.3", true},
		// Conjunction and disjunction.
		{"1.5.0", ">=1.2.3 <2.0.0", true},
		{"2.0.1", ">=1.2.3 <2.0.0", false},
		{"2.5.0", "^1.0.0 || ^2.0.0", true},
		{"3.0.0", "^1.0.0 || ^2.0.0", false},
		// Wildcards match anything.
		{"9.9.9", "*", true},
		{"9.9.9", "", true},
		// Partial versions behave as a range over what is unspecified.
		{"1.2.9", "1.2", true},
		{"1.3.0", "1.2", false},
		// Prereleases stay out of ordinary ranges, and opt in when named.
		{"2.0.0-beta.1", "^1.0.0", false},
		{"1.9.0-beta.1", "^1.0.0", false},
		{"1.9.0-beta.1", ">=1.9.0-beta.1", true},
		// An unparseable constraint must match nothing rather than everything.
		{"1.0.0", "not-a-range", false},
		{"1.0.0", "1.2.3 - 2.0.0", false},
	} {
		if got := satisfies(tc.version, tc.constraint); got != tc.want {
			t.Errorf("satisfies(%q,%q)=%v want %v", tc.version, tc.constraint, got, tc.want)
		}
	}
}

func TestNewestInRangeAndNewestPublished(t *testing.T) {
	versions := []string{"0.0.27", "0.0.31", "0.0.32", "0.0.33", "0.1.0", "0.1.1-beta.1"}

	// The regression: a caret pin on a 0.0.x version selects one version, which
	// is exactly why "is there something newer in range" used to answer no.
	if got, ok := newestInRange(versions, "^0.0.31"); !ok || got != "0.0.31" {
		t.Errorf("newestInRange(^0.0.31)=%q,%v want 0.0.31", got, ok)
	}
	if got, ok := newestInRange(versions, "~0.0.31"); !ok || got != "0.0.33" {
		t.Errorf("newestInRange(~0.0.31)=%q,%v want 0.0.33", got, ok)
	}
	if got, ok := newestInRange(versions, "^0.1.0"); !ok || got != "0.1.0" {
		t.Errorf("newestInRange(^0.1.0)=%q,%v want 0.1.0", got, ok)
	}
	if _, ok := newestInRange(versions, "^2.0.0"); ok {
		t.Error("newestInRange must report no match outside the range")
	}

	// The optimistic view looks past every range, but still skips prereleases.
	if got, ok := newestPublished(versions); !ok || got != "0.1.0" {
		t.Errorf("newestPublished=%q,%v want 0.1.0", got, ok)
	}
	if _, ok := newestPublished([]string{"1.0.0-beta.1"}); ok {
		t.Error("a prerelease-only list has no newest release")
	}
}

func TestSortVersionsNewestFirst(t *testing.T) {
	versions := []string{"1.8.0", "1.12.0", "1.9.0"}
	sortVersions(versions)
	if want := []string{"1.12.0", "1.9.0", "1.8.0"}; !slices.Equal(versions, want) {
		t.Fatalf("versions=%v want %v", versions, want)
	}
	// Fork builds carry +fork.<sha> metadata. Semver gives build metadata no
	// precedence, so a fork build ranks with the release it was built from rather
	// than above or below it; the plain release is listed first for stability.
	mixed := []string{"0.0.33+fork.abc123", "0.0.31", "0.0.33"}
	sortVersions(mixed)
	if want := []string{"0.0.33", "0.0.33+fork.abc123", "0.0.31"}; !slices.Equal(mixed, want) {
		t.Fatalf("mixed=%v want %v", mixed, want)
	}
	// Anything unparseable is kept and sorted last rather than dropped.
	junk := []string{"not-a-version", "1.0.0", "0.9.0"}
	sortVersions(junk)
	if want := []string{"1.0.0", "0.9.0", "not-a-version"}; !slices.Equal(junk, want) {
		t.Fatalf("junk=%v want %v", junk, want)
	}
}

func TestPickUpdateIsOptimisticButPrefersCompatible(t *testing.T) {
	const pi = "pi-acp"
	for _, tc := range []struct {
		name, constraint, current string
		versions                  []string
		wantOffer                 string
		wantCompatible            bool
		wantNewestPublished       string
		wantOK                    bool
	}{
		{
			// The reported bug: a caret pin on 0.0.x froze the range at the
			// installed version, so no update was ever offered. Optimism means the
			// newer release is surfaced anyway, flagged as beyond the range.
			name:       "offers beyond a range that admits nothing newer",
			constraint: "^0.0.31", current: "0.0.31",
			versions:  []string{"0.0.31", "0.0.32", "0.0.33"},
			wantOffer: "0.0.33", wantCompatible: false, wantOK: true,
		},
		{
			name:       "prefers the newest compatible version when one exists",
			constraint: "~0.0.31", current: "0.0.31",
			versions:  []string{"0.0.31", "0.0.33", "0.1.0"},
			wantOffer: "0.0.33", wantCompatible: true, wantNewestPublished: "0.1.0", wantOK: true,
		},
		{
			name:       "no offer when already on the newest release",
			constraint: "~0.0.33", current: "0.0.33",
			versions: []string{"0.0.31", "0.0.33"},
			wantOK:   false,
		},
		{
			name:       "no offer when the installed version is ahead of npm",
			constraint: "^1.8.0", current: "1.13.0",
			versions: []string{"1.8.0", "1.12.0"},
			wantOK:   false,
		},
		{
			name:       "an in-range upgrade that is also the newest reports no extra",
			constraint: "^1.8.0", current: "1.8.0",
			versions:  []string{"1.8.0", "1.12.0"},
			wantOffer: "1.12.0", wantCompatible: true, wantOK: true,
		},
		{
			name:       "prereleases are never offered",
			constraint: "^1.8.0", current: "1.8.0",
			versions:  []string{"1.8.0", "1.9.0", "2.0.0-beta.1"},
			wantOffer: "1.9.0", wantCompatible: true, wantOK: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickUpdate("pi", pi, tc.constraint, tc.current, tc.versions)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (offer %q)", ok, tc.wantOK, got.LatestVersion)
			}
			if !ok {
				return
			}
			if got.LatestVersion != tc.wantOffer {
				t.Errorf("offer=%q want %q", got.LatestVersion, tc.wantOffer)
			}
			if got.Compatible != tc.wantCompatible {
				t.Errorf("compatible=%v want %v", got.Compatible, tc.wantCompatible)
			}
			if got.NewestPublished != tc.wantNewestPublished {
				t.Errorf("newestPublished=%q want %q", got.NewestPublished, tc.wantNewestPublished)
			}
			if got.Kind != UpdateKindRelease {
				t.Errorf("kind=%q want %q", got.Kind, UpdateKindRelease)
			}
		})
	}
}

// TestShippedPinsAdmitFutureReleases guards the mistake that started this: a
// pin whose range can never admit anything newer makes the update check inert
// for that agent, silently and forever.
func TestShippedPinsAdmitFutureReleases(t *testing.T) {
	for agent, p := range agentPins {
		constraint := trimConstraint(p)
		r, ok := parseRange(constraint)
		if !ok {
			t.Errorf("%s: pin %q is not a parseable npm range", agent, constraint)
			continue
		}
		base, _, ok := parsePartial(strings.TrimLeft(constraint, "^~>=< "))
		if !ok {
			t.Errorf("%s: cannot read a base version out of pin %q", agent, constraint)
			continue
		}
		// Some release after the pinned base must be admissible, or the range is
		// a single point and no update can ever be detected.
		next := semver{major: base.major, minor: base.minor, patch: base.patch + 1}
		alsoNext := semver{major: base.major, minor: base.minor + 1}
		if !r.matches(next) && !r.matches(alsoNext) {
			t.Errorf("%s: pin %q admits no release after %d.%d.%d, so updates can never be detected (use ~ instead of ^ on a 0.0.x package)",
				agent, constraint, base.major, base.minor, base.patch)
		}
	}
}

func trimConstraint(p pin) string {
	return strings.TrimPrefix(p.spec, p.packageName+"@")
}
