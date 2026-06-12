// Package version reports the build identity of the driver binary.
package version

import (
	"fmt"
	"runtime"
)

// Stamped by the linker at build time; see the Makefile. The defaults are what an unstamped
// go build or go install produces.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// Info is the build identity of this binary.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
	GoVersion string
	Platform  string
}

// Get returns the stamped build identity together with the toolchain and target it was built for.
func Get() Info {
	return Info{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// Version returns the release version reported through the CSI Identity service.
func Version() string {
	return version
}

// String summarizes the build identity on one line, without the program name.
func (i Info) String() string {
	return fmt.Sprintf(
		"%s (commit %s, built %s, %s, %s)",
		i.Version,
		i.Commit,
		i.BuildDate,
		i.GoVersion,
		i.Platform,
	)
}
