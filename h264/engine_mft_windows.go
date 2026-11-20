//go:build windows && (amd64 || arm64)

package h264

func init() { platform = mftEngine{sys: winMFT{}} }
