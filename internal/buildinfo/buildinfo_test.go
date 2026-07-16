package buildinfo

import "testing"

func TestDevelopmentMetadata(t *testing.T) {
	oldVersion, oldCommit, oldBuildTime := Version, Commit, BuildTime
	t.Cleanup(func() { Version, Commit, BuildTime = oldVersion, oldCommit, oldBuildTime })
	Version, Commit, BuildTime = "dev", "unknown", "unknown"

	if got, want := String(), "tandem version=dev commit=unknown built=unknown"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
