//go:build darwin && arm64

package native

import _ "embed"

//go:embed libkolovpx-darwin-arm64.dylib
var libData []byte
