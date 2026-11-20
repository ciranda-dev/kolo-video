//go:build !((darwin && (amd64 || arm64)) || (windows && (amd64 || arm64)) || (linux && (amd64 || arm64) && kolovideo_native))

package native

var libData []byte
