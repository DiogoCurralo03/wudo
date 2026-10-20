//go:build wudo_installer

package main

import (
	"embed"
	"fmt"
)

// payloadFS carries the binaries to install. The files are staged by
// scripts/build.ps1 right before this package is compiled with the
// wudo_installer tag.
//
//go:embed payload/wudo.exe payload/wudosvc.exe
var payloadFS embed.FS

// loadPayload reads the embedded executables.
func loadPayload() (map[string][]byte, error) {
	out := make(map[string][]byte, len(payloadNames))
	for _, name := range payloadNames {
		data, err := payloadFS.ReadFile("payload/" + name)
		if err != nil {
			return nil, fmt.Errorf("the installer is incomplete, %s is missing: %w", name, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("the installer is incomplete, %s is empty", name)
		}
		out[name] = data
	}
	return out, nil
}
