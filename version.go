package main

import "runtime/debug"

// moduleVersion is the version `go install github.com/bytes032/land@vX` recorded in the binary, else version.
func moduleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
