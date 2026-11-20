//go:build !darwin

package native

func bundleLibrary() string       { return "" }
func checkSignature(string) error { return nil }

func openBundle(string) (*Lib, error) { return nil, nil }
