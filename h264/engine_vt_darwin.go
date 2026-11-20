//go:build darwin && (amd64 || arm64)

package h264

func init() { platform = &vtEngine{load: loadVT} }
