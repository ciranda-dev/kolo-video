//go:build darwin && amd64

package native

import _ "embed"

//go:embed libkolovpx-darwin-amd64.dylib
var libData []byte
