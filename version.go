// Stolen from: https://github.com/shutter-network/rolling-shutter/blob/f21a5db5b27894dddc384d975418397c8858832e/rolling-shutter/cmd/shversion/shversion.go
package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var version string

// Version returns shuttermint's version string.
func Version() string {
	return fmt.Sprintf("%s (%s, %s-%s)", VersionShort(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func VersionShort() string {
	if version == "" {
		info, ok := debug.ReadBuildInfo()
		if ok {
			version = info.Main.Version
			if version == "(devel)" {
				for _, s := range info.Settings {
					if s.Key == "vcs.revision" {
						version = fmt.Sprintf("(devel-%s)", s.Value)
						break
					}
				}
			}
		}
	}
	return version
}
