//go:build wudo_installer

package main

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"fmt"
	"io"
)

// payloadZip carries the compressed binaries to install. Compressing the payload
// prevents antivirus heuristic scanners (such as Microsoft Defender ML models)
// from flagging the raw embedded PE executables as a dropper trojan.
//
//go:embed payload/payload.zip
var payloadZip []byte

// loadPayload reads and extracts the embedded executables.
func loadPayload() (map[string][]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(payloadZip), int64(len(payloadZip)))
	if err != nil {
		return nil, fmt.Errorf("read payload archive: %w", err)
	}
	out := make(map[string][]byte, len(r.File))
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s in payload: %w", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s in payload: %w", f.Name, err)
		}
		out[f.Name] = data
	}
	for _, name := range payloadNames {
		if len(out[name]) == 0 {
			return nil, fmt.Errorf("the installer is incomplete, %s is missing", name)
		}
	}
	return out, nil
}
