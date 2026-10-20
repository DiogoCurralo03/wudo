package setup

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAddToPathValueIsIdempotent checks that installing twice does not duplicate
// the PATH entry.
func TestAddToPathValueIsIdempotent(t *testing.T) {
	base := `C:\Windows\System32;C:\Tools`
	updated, changed := addToPathValue(base, `C:\wudo\bin`)
	if !changed {
		t.Fatal("the first call did not change the value")
	}
	if updated != base+`;C:\wudo\bin` {
		t.Errorf("value = %q, want it to append the folder", updated)
	}
	again, changed := addToPathValue(updated, `C:\wudo\bin`)
	if changed {
		t.Errorf("the second call changed the value again: %q", again)
	}
}

// TestAddToPathValueNormalisesSpelling checks the many ways the same folder can
// already appear in PATH.
func TestAddToPathValueNormalisesSpelling(t *testing.T) {
	for _, existing := range []string{
		`C:\wudo\bin`,
		`c:\wudo\bin`,
		`C:\wudo\bin\`,
		`"C:\wudo\bin"`,
		` C:\wudo\bin `,
	} {
		value := `C:\Windows;` + existing
		if _, changed := addToPathValue(value, `C:\wudo\bin`); changed {
			t.Errorf("PATH with %q was modified, it should already count as present", existing)
		}
	}
}

// TestAddToPathValueHandlesEmpty checks the very first entry of an empty PATH.
func TestAddToPathValueHandlesEmpty(t *testing.T) {
	updated, changed := addToPathValue("", `C:\wudo\bin`)
	if !changed {
		t.Fatal("an empty PATH was not filled")
	}
	if updated != `C:\wudo\bin` {
		t.Errorf("value = %q, want just the folder", updated)
	}
}

// TestAddToPathValueKeepsTrailingSemicolon checks that a value already ending in
// a separator does not get a doubled one.
func TestAddToPathValueKeepsTrailingSemicolon(t *testing.T) {
	updated, _ := addToPathValue(`C:\Windows;`, `C:\wudo\bin`)
	if updated != `C:\Windows;C:\wudo\bin` {
		t.Errorf("value = %q", updated)
	}
}

// TestRemoveFromPathValue checks removal, including the spellings above.
func TestRemoveFromPathValue(t *testing.T) {
	for _, existing := range []string{
		`C:\wudo\bin`,
		`c:\wudo\bin`,
		`C:\wudo\bin\`,
		`"C:\wudo\bin"`,
	} {
		value := `C:\Windows;` + existing + `;C:\Tools`
		updated, removed := removeFromPathValue(value, `C:\wudo\bin`)
		if !removed {
			t.Errorf("PATH with %q was not cleaned", existing)
			continue
		}
		if strings.Contains(strings.ToLower(updated), "wudo") {
			t.Errorf("value = %q, it still mentions wudo", updated)
		}
		if !strings.Contains(updated, `C:\Windows`) || !strings.Contains(updated, `C:\Tools`) {
			t.Errorf("value = %q, the other entries must survive", updated)
		}
	}
}

// TestRemoveFromPathValueMissing checks that removing an absent folder is a no-op.
func TestRemoveFromPathValueMissing(t *testing.T) {
	value := `C:\Windows;C:\Tools`
	if _, removed := removeFromPathValue(value, `C:\wudo\bin`); removed {
		t.Error("removal reported a change on a PATH without the folder")
	}
}

// TestTaskXMLIsWellFormed checks the logon task definition: highest run level (no
// UAC prompt), interactive token (tray icon in the user session) and the quoted
// service path.
func TestTaskXMLIsWellFormed(t *testing.T) {
	xml := taskXML(`C:\Program Files\wudo\bin\wudosvc.exe`)
	for _, want := range []string{
		"<RunLevel>HighestAvailable</RunLevel>",
		"<LogonType>InteractiveToken</LogonType>",
		"<LogonTrigger>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		`<Command>C:\Program Files\wudo\bin\wudosvc.exe</Command>`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("task definition is missing %q", want)
		}
	}
	if strings.Contains(xml, "&amp;amp;") {
		t.Error("the escaping was applied twice")
	}
}

// TestTaskXMLEscapesAmpersand checks folder names with special characters.
func TestTaskXMLEscapesAmpersand(t *testing.T) {
	xml := taskXML(`C:\Tools & Stuff\wudosvc.exe`)
	if !strings.Contains(xml, `C:\Tools &amp; Stuff\wudosvc.exe`) {
		t.Errorf("the ampersand was not escaped:\n%s", xml)
	}
}

// TestEncodeUTF16LE checks the BOM and the encoding schtasks expects.
func TestEncodeUTF16LE(t *testing.T) {
	data := encodeUTF16LE("A")
	if len(data) != 4 {
		t.Fatalf("length = %d, want 4 (BOM + one code unit)", len(data))
	}
	if data[0] != 0xFF || data[1] != 0xFE {
		t.Errorf("missing UTF-16LE BOM: % x", data[:2])
	}
	if data[2] != 'A' || data[3] != 0 {
		t.Errorf("encoding is wrong: % x", data[2:])
	}
}

// TestTaskXMLParsesAsXML checks that the task definition is well formed and
// carries the declaration schtasks expects for a UTF-16 file.
func TestTaskXMLParsesAsXML(t *testing.T) {
	data := encodeUTF16LE(taskXML(`C:\Program Files\wudo\bin\wudosvc.exe`))
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xFE {
		t.Fatal("missing BOM")
	}
	// decode UTF-16LE manually
	var s []rune
	for i := 2; i+1 < len(data); i += 2 {
		s = append(s, rune(uint16(data[i])|uint16(data[i+1])<<8))
	}
	text := string(s)
	if !strings.HasPrefix(text, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Errorf("the declaration is missing:\n%s", text[:80])
	}
	// Go's decoder refuses the UTF-16 declaration, but the body must be
	// well formed XML on its own.
	body := text[strings.Index(text, "<Task"):]
	var root struct {
		XMLName xml.Name
		Version string `xml:"version,attr"`
	}
	if err := xml.Unmarshal([]byte(body), &root); err != nil {
		t.Fatalf("the task definition is not valid XML: %v", err)
	}
	if root.XMLName.Local != "Task" {
		t.Errorf("root element = %q, want Task", root.XMLName.Local)
	}
	if _, err := os.Stat(os.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// TestBinDirFor checks the folder layout.
func TestBinDirFor(t *testing.T) {
	if got := BinDirFor(`C:\wudo`); got != filepath.Join(`C:\wudo`, "bin") {
		t.Errorf("BinDirFor = %q", got)
	}
	if got := BinDirFor(""); !strings.HasSuffix(got, "bin") {
		t.Errorf("BinDirFor(\"\") = %q", got)
	}
}
