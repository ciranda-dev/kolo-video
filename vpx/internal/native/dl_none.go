//go:build !((darwin || windows || (linux && kolovideo_native)) && (amd64 || arm64))

package native

import "runtime"

const dlAvailable = false

func notBuilt() string {
	if runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		return "built without the kolovideo_native tag"
	}
	return ""
}

func dlOpen(string) (uintptr, error)                          { return 0, ErrUnsupported }
func dlClose(uintptr)                                         {}
func bind(uintptr, *Lib) (func() int32, func() string, error) { return nil, nil, ErrUnsupported }
func platformHint(error) string                               { return "" }
