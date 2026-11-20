//go:build windows && amd64

package native

import _ "embed"

//go:embed libkolovpx-windows-amd64.dll
var libData []byte
