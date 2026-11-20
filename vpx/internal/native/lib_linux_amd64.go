//go:build linux && amd64 && kolovideo_native

package native

import _ "embed"

//go:embed libkolovpx-linux-amd64.so
var libData []byte
