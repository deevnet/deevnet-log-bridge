// Package version carries what the build stamped in.
package version

// Set with -ldflags at build time; the defaults say "not built by make".
var (
	Version = "dev"
	Commit  = "unknown"
	Built   = "unknown"
)
