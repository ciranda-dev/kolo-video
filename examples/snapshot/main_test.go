package main

import (
	"image"
	"os"
	"path/filepath"
	"testing"
)

func TestSavePNG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frame-000.png")
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	if err := savePNG(path, img); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	if err := savePNG(path, img); err == nil {
		t.Error("an existing file was replaced")
	}
}
