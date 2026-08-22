package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func reset() { fileValues = map[string]string{} }

func TestLoadFileParsesTheUsualShapes(t *testing.T) {
	reset()
	t.Cleanup(reset)

	path := writeTemp(t, "config.env", `
# a comment
RELAY_LOG_LEVEL=debug
export EXIT_COUNTRY=NL,DE
QUOTED="  spaces kept  "
EMPTY=
WITH_EQUALS=a=b=c
`)
	if err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"RELAY_LOG_LEVEL": "debug",
		"EXIT_COUNTRY":    "NL,DE",
		"QUOTED":          "  spaces kept  ",
		"EMPTY":           "",
		"WITH_EQUALS":     "a=b=c",
	} {
		if got := fileValues[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	reset()
	t.Cleanup(reset)
	if err := LoadFile(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Fatalf("a missing config file must not be fatal: %v", err)
	}
}

func TestMalformedLineIsReportedWithItsNumber(t *testing.T) {
	reset()
	t.Cleanup(reset)
	path := writeTemp(t, "bad.env", "GOOD=1\nthis is not a setting\n")
	err := LoadFile(path)
	if err == nil {
		t.Fatal("expected an error for a malformed line")
	}
	if got := err.Error(); got == "" || !contains(got, ":2:") {
		t.Errorf("error should name the offending line, got %q", got)
	}
}

func TestEnvironmentBeatsTheFile(t *testing.T) {
	reset()
	t.Cleanup(reset)
	path := writeTemp(t, "config.env", "RELAY_LOG_LEVEL=debug\n")
	if err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
	if got := env("RELAY_LOG_LEVEL", "info"); got != "debug" {
		t.Fatalf("file value not used: %q", got)
	}
	t.Setenv("RELAY_LOG_LEVEL", "warn")
	if got := env("RELAY_LOG_LEVEL", "info"); got != "warn" {
		t.Errorf("environment must win over the file, got %q", got)
	}
}

// An empty passphrase is legitimate - the Mysterium desktop app uses one - so
// it must be distinguishable from "not configured".
func TestEmptyPassphraseInFileCountsAsSet(t *testing.T) {
	reset()
	t.Cleanup(reset)
	path := writeTemp(t, "config.env", "MYST_IDENTITY_PASSPHRASE=\n")
	if err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
	got, err := secret("MYST_IDENTITY_PASSPHRASE")
	if err != nil {
		t.Fatalf("an empty passphrase must be accepted: %v", err)
	}
	if got != "" {
		t.Errorf("passphrase = %q, want empty", got)
	}
}

func TestParseForwards(t *testing.T) {
	got, err := parseForwards("127.0.0.1:1090", "example.com:443", "0.0.0.0:1091=host:22")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d forwards, want 2: %+v", len(got), got)
	}
	if got[0].Target != "example.com:443" || got[1].Listen != "0.0.0.0:1091" {
		t.Errorf("unexpected forwards: %+v", got)
	}

	if _, err := parseForwards("127.0.0.1:1090", "", ""); err == nil {
		t.Error("a raw listener without a target must be rejected")
	}
	if _, err := parseForwards("", "", "not-a-pair"); err == nil {
		t.Error("a malformed forward must be rejected")
	}
	if _, err := parseForwards("", "", "1.2.3.4:5=noport"); err == nil {
		t.Error("a target without a port must be rejected")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
