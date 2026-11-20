//go:build !(darwin && (amd64 || arm64)) && !(windows && (amd64 || arm64)) && !linux

package h264

import "runtime"

func init() {
	platform = noEngine{&UnavailableError{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Reason: "no H.264 engine on " + runtime.GOOS + "/" + runtime.GOARCH,
	}}
}
