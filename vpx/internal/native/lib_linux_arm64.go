//go:build linux && arm64 && kolovideo_native

package native

import _ "embed"

//go:embed libkolovpx-linux-arm64.so
var libData []byte
