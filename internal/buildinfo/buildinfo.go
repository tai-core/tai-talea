// Package buildinfo exposes the build metadata of the control plane binary.
package buildinfo

import "runtime"

// Values are overridden at build time with -ldflags "-X ...".
var (
	// Version is the release version.
	Version = "dev"
	// Commit is the source revision.
	Commit = "unknown"
	// BuildTime is the link timestamp.
	BuildTime = "unknown"
)

// Summary renders a one line build description.
func Summary() string {
	return Version + " (" + Commit + ", " + BuildTime + ", " + runtime.Version() + ")"
}
