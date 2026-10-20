//go:build !wudo_installer

package main

import "errors"

// loadPayload in a build without the embedded binaries. The real installer is
// produced by scripts/build.ps1, which stages the payload and rebuilds this
// package with the wudo_installer tag. Keeping this file lets plain
// "go build ./..." and "go test ./..." work in a fresh checkout.
func loadPayload() (map[string][]byte, error) {
	return nil, errors.New("this build carries no binaries; build the installer with scripts\\build.ps1")
}
