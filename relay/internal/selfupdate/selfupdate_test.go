package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		dev   bool
		major int
		patch int
	}{
		{"v0.4.0", true, false, 0, 0},
		{"0.4.0", true, false, 0, 0},
		{"v1.39.5", true, false, 1, 5},
		{"v0.4.0-3-gc0ffee", true, true, 0, 0},
		{"v0.4.0-dirty", true, true, 0, 0},
		{"v0.4.0-3-gc0ffee-dirty", true, true, 0, 0},
		{"v0.5.0-rc1", true, false, 0, 0},
		{"dev", false, false, 0, 0},
		{"", false, false, 0, 0},
		{"c0ffee", false, false, 0, 0},
		{"v0.4", false, false, 0, 0},
		{"v0.4.x", false, false, 0, 0},
	}
	for _, c := range cases {
		got := Parse(c.in)
		if got.OK != c.ok || got.Dev != c.dev {
			t.Errorf("Parse(%q) = %+v, want ok=%v dev=%v", c.in, got, c.ok, c.dev)
		}
		if c.ok && (got.Major != c.major || got.Patch != c.patch) {
			t.Errorf("Parse(%q) = %+v, want major=%d patch=%d", c.in, got, c.major, c.patch)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.4.0", "v0.5.0", -1},
		{"v0.5.0", "v0.4.0", 1},
		{"v0.4.0", "v0.4.0", 0},
		{"v0.4.0", "v0.4.1", -1},
		{"v0.9.0", "v1.0.0", -1},
		{"v1.2.3", "v1.10.0", -1},
		// A build made after a tag is newer than that tag, and still older
		// than the next release.
		{"v0.4.0-3-gc0ffee", "v0.4.0", 1},
		{"v0.4.0-dirty", "v0.4.0", 1},
		{"v0.4.0-3-gc0ffee", "v0.5.0", -1},
		// A pre-release is older than the release it leads to.
		{"v0.5.0-rc1", "v0.5.0", -1},
		{"v0.5.0", "v0.5.0-rc1", 1},
	}
	for _, c := range cases {
		if got := Parse(c.a).Compare(Parse(c.b)); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestVersionString(t *testing.T) {
	if got := Parse("0.4.0-dirty").String(); got != "v0.4.0-dirty" {
		t.Errorf("String() = %q, want v0.4.0-dirty", got)
	}
	if got := Parse("dev").String(); got != "" {
		t.Errorf("String() of an unparsed version = %q, want empty", got)
	}
}

func TestParseSums(t *testing.T) {
	digest := "2e339b0ad336968662de55d3298ce2793602ef0d808cafe6e259bfa672576047"
	sums := ParseSums([]byte(
		digest + "  mystrelay\n" +
			"0E113F75865B673DF06E58E40589266569F30E773F1395D70C8CD1BD1BF2BEE5 *mystrelay.exe\n" +
			"# a comment\n" +
			"short  ignored\n\n"))
	if sums["mystrelay"] != digest {
		t.Errorf("mystrelay = %q, want %q", sums["mystrelay"], digest)
	}
	if sums["mystrelay.exe"] != "0e113f75865b673df06e58e40589266569f30e773f1395d70c8cd1bd1bf2bee5" {
		t.Errorf("a binary-mode digest was not read: %q", sums["mystrelay.exe"])
	}
	if len(sums) != 2 {
		t.Errorf("read %d digests, want 2: %v", len(sums), sums)
	}
}

func TestAssetNames(t *testing.T) {
	if got := AssetNames("windows", "amd64"); got[0] != "mystrelay_windows_amd64.exe" || got[1] != "mystrelay.exe" {
		t.Errorf("windows/amd64 asset names = %v", got)
	}
	if got := AssetNames("linux", "amd64"); got[0] != "mystrelay_linux_amd64" || got[1] != "mystrelay" {
		t.Errorf("linux/amd64 asset names = %v", got)
	}
	// An architecture the releases have never shipped must not fall back to
	// the untagged name, which is an amd64 binary.
	if got := AssetNames("linux", "arm64"); len(got) != 1 || got[0] != "mystrelay_linux_arm64" {
		t.Errorf("linux/arm64 asset names = %v, want only the tagged name", got)
	}
}

// release serves a GitHub-shaped release and its assets.
func release(t *testing.T, payload []byte) (*httptest.Server, *Client) {
	t.Helper()
	digest := sha256.Sum256(payload)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("the request carried no User-Agent, which GitHub refuses")
		}
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","html_url":"https://example.invalid/r","published_at":"2026-08-23T10:00:00Z",
			"assets":[{"name":"mystrelay","browser_download_url":%q},{"name":"SHA256SUMS","browser_download_url":%q}]}`,
			srv.URL+"/dl/mystrelay", srv.URL+"/dl/SHA256SUMS")
	})
	mux.HandleFunc("/dl/mystrelay", func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  mystrelay\n", hex.EncodeToString(digest[:]))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &Client{HTTP: srv.Client(), Repo: "o/r", API: srv.URL, UserAgent: "myst-relay/test"}
}

func TestLatestAndDownload(t *testing.T) {
	payload := []byte("#!/bin/sh\necho myst-relay v9.9.9\n")
	_, c := release(t, payload)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rel, err := c.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Tag != "v9.9.9" || rel.Page != "https://example.invalid/r" {
		t.Fatalf("release = %+v", rel)
	}
	if rel.Published.Year() != 2026 {
		t.Errorf("published = %v", rel.Published)
	}

	name, assetURL, err := rel.Binary("linux", "amd64")
	if err != nil {
		t.Fatalf("Binary: %v", err)
	}
	if name != "mystrelay" {
		t.Fatalf("asset name = %q", name)
	}
	sumsURL, err := rel.Sums()
	if err != nil {
		t.Fatalf("Sums: %v", err)
	}
	body, err := c.Fetch(ctx, sumsURL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := ParseSums(body)[name]
	if want == "" {
		t.Fatalf("no digest for %q in %q", name, body)
	}

	dir := t.TempDir()
	path, err := c.Download(ctx, assetURL, dir, want)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("downloaded %q, %v", got, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm()&0o100 == 0 {
		t.Errorf("the download is not executable: %v", st.Mode())
	}

	// A digest that does not match must leave nothing behind.
	if _, err := c.Download(ctx, assetURL, dir, "00"+want[2:]); err == nil {
		t.Fatal("Download accepted a file that did not match its digest")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("a rejected download was left in place: %v", entries)
	}
}

func TestMissingAssets(t *testing.T) {
	rel := &Release{Tag: "v9.9.9", Assets: map[string]string{"mystrelay": "u"}}
	if _, err := rel.Sums(); err == nil {
		t.Error("a release without SHA256SUMS was accepted")
	}
	if _, _, err := rel.Binary("windows", "amd64"); err == nil {
		t.Error("a release without a Windows binary was accepted")
	}
}

func TestReleaseNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Repo: "o/r", API: srv.URL}
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("a 404 was not reported")
	}
}

func TestReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mystrelay")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "download")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Replace(src, target); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Fatalf("target holds %q, %v", got, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("the download was left behind: %v", err)
	}

	// CleanOld is a no-op when there is nothing to clean.
	CleanOld(target)
	if _, err := os.Stat(target); err != nil {
		t.Errorf("CleanOld removed the binary itself: %v", err)
	}
}
