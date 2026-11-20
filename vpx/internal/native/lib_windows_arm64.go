//go:build windows && arm64

package native

import _ "embed"

//go:embed libkolovpx-windows-arm64.dll
var libData []byte
