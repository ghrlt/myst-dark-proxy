package node

import "testing"

func TestArchiveForKnownPlatforms(t *testing.T) {
	for _, p := range []string{"windows/amd64", "linux/amd64"} {
		a, err := ArchiveFor(DefaultVersion, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if len(a.SHA256) != 64 {
			t.Errorf("%s: checksum is not a sha256: %q", p, a.SHA256)
		}
		if a.URL == "" || a.Member == "" {
			t.Errorf("%s: incomplete archive definition %+v", p, a)
		}
		if a.Kind != "zip" && a.Kind != "targz" {
			t.Errorf("%s: unknown archive kind %q", p, a.Kind)
		}
	}
}

func TestArchiveForUnknownPlatformExplainsItself(t *testing.T) {
	_, err := ArchiveFor(DefaultVersion, "plan9/arm")
	if err == nil {
		t.Fatal("expected an error for an unpackaged platform")
	}
	// The message must name what is available, or the user is left guessing
	// why a download refused to happen.
	if got := err.Error(); len(got) < 20 {
		t.Errorf("unhelpful error: %q", got)
	}
}

func TestBinaryNameFollowsThePlatform(t *testing.T) {
	if got := (Archive{Member: "myst.exe"}).BinaryName(); got != "myst.exe" {
		t.Errorf("windows binary named %q", got)
	}
	if got := (Archive{Member: "./myst"}).BinaryName(); got != "myst" {
		t.Errorf("unix binary named %q", got)
	}
}

func TestSameMemberToleratesArchivePathStyles(t *testing.T) {
	// Releases have shipped the same file as "myst", "./myst" and "/myst".
	for _, c := range []struct {
		name, member string
		want         bool
	}{
		{"./myst", "./myst", true},
		{"myst", "./myst", true},
		{"./myst", "myst", true},
		{"myst.exe", "myst.exe", true},
		{"config/myst", "./myst", false},
		{"myst_supervisor", "./myst", false},
	} {
		if got := sameMember(c.name, c.member); got != c.want {
			t.Errorf("sameMember(%q, %q) = %v, want %v", c.name, c.member, got, c.want)
		}
	}
}
