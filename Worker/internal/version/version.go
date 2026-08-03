// Package version exposes build-time metadata for the Worker binary.
package version

// Version is the semantic version of the Worker binary. Overridden at build
// time via -ldflags "-X gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/version.Version=...".
var Version = "0.0.0-dev"
