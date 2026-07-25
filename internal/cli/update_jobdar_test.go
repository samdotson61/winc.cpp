package cli

import "testing"

// The winc-jobdar branch is jobfaro's inference backend. Its whole reason to
// exist is that `winc serve --eval` stays stable here while master moves, so
// the one thing that must never happen is a jobdar binary quietly replacing
// itself with a master release build -- that would drop the eval profile under
// a running jobfaro install with no error and no visible version change beyond
// a suffix disappearing.
//
// The only thing standing between that and a user is isJobdarBuild(), keyed on
// the "-jobdar.N" suffix in version.go. It had no test: renaming the suffix,
// or switching Version to a struct/ldflags-only form, would disarm the guard
// silently and every gate would still pass. These tests make that a build
// failure instead.

func TestIsJobdarBuildRecognisesBranchVersions(t *testing.T) {
	// The real form the branch has shipped since 1.21.2, plus the shapes a
	// future merge could produce.
	for _, v := range []string{
		"1.31.0-jobdar.1",
		"1.30.0-jobdar.1",
		"1.28.0-jobdar.3",
		"1.21.3-jobdar.4",
		"2.0.0-jobdar.10",
	} {
		t.Run(v, func(t *testing.T) {
			orig := Version
			defer func() { Version = orig }()
			Version = v
			if !isJobdarBuild() {
				t.Fatalf("isJobdarBuild() = false for %q; the self-update guard is disarmed and a "+
					"master release would overwrite this jobfaro backend", v)
			}
		})
	}
}

func TestIsJobdarBuildLeavesMasterAlone(t *testing.T) {
	// Master builds must still self-update -- the guard has to be specific, not
	// a blanket refusal, or prebuilt installs strand on old code (the bug
	// v1.18.0 shipped to fix).
	for _, v := range []string{
		"1.31.0",
		"1.30.0",
		"1.14.1",
	} {
		t.Run(v, func(t *testing.T) {
			orig := Version
			defer func() { Version = orig }()
			Version = v
			if isJobdarBuild() {
				t.Fatalf("isJobdarBuild() = true for master version %q; prebuilt master installs "+
					"would stop self-updating", v)
			}
		})
	}
}

// The guard is only load-bearing if THIS branch's committed version actually
// carries the suffix it keys on. A merge that resolved version.go the wrong way
// -- taking master's plain "1.31.0" instead of keeping "-jobdar.1" -- would
// leave every other test green while shipping a branch binary that self-updates
// itself into a master release. That resolution has to be made by hand on every
// master merge, so it is exactly the step worth pinning.
func TestBranchVersionCarriesJobdarSuffix(t *testing.T) {
	if !isJobdarBuild() {
		t.Fatalf("this branch's Version is %q, which does not identify a jobdar build. "+
			"If this fired after merging master, version.go took master's version verbatim -- "+
			"restore the -jobdar.N suffix on the new base.", Version)
	}
}
