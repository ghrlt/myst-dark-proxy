// Package selfupdate installs a newer release of this binary over the one that
// is running.
//
// Releases are published on GitHub as one binary per platform plus a
// SHA256SUMS file, and that digest list is what a download is checked against
// before anything is replaced. The check is the whole trust boundary: it
// catches a truncated or corrupted transfer, not a compromised release, since
// the binary and its digest come from the same place. TLS to GitHub is what
// says the release is the project's.
//
// Nothing here runs on a timer. A relay carrying traffic makes no request of
// its own, so the only thing that ever tells GitHub this machine exists is
// someone typing "mystrelay update".
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultRepo is where releases of this program are published.
	DefaultRepo = "ghrlt/myst-dark-proxy"

	// SumsAsset is the digest list every release carries.
	SumsAsset = "SHA256SUMS"

	defaultAPI = "https://api.github.com"

	// maxAsset bounds what a download may write. The binary is ~7 MiB; this
	// is room to grow and a limit on a server that answers with a firehose.
	maxAsset = 64 << 20
)

// --- versions --------------------------------------------------------------

// Version is a release version.
//
// Releases are tagged vMAJOR.MINOR.PATCH. A binary built from a working tree
// carries what "git describe" produced instead, which is the last tag plus how
// far past it the build is - "v0.4.0-3-gc0ffee", or "v0.4.0-dirty". Such a
// build is *ahead* of its tag, the opposite of what semver's rule for a suffix
// would say about "v0.4.0-rc1", so the two kinds of suffix are told apart
// rather than compared as one.
type Version struct {
	Major, Minor, Patch int
	Suffix              string // everything after the numbers, without the dash
	Dev                 bool   // the suffix is one git describe appends
	OK                  bool   // parsed as a version at all
}

// Parse reads a version from a tag or a build stamp. A stamp that is not a
// version at all - "dev", a bare commit - parses as not OK rather than as an
// error, because "unknown" is a real answer here.
func Parse(s string) Version {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return Version{}
	}
	numbers, suffix, _ := strings.Cut(s, "-")
	parts := strings.Split(numbers, ".")
	if len(parts) != 3 {
		return Version{}
	}
	v := Version{Suffix: suffix, OK: true}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}
		}
		switch i {
		case 0:
			v.Major = n
		case 1:
			v.Minor = n
		case 2:
			v.Patch = n
		}
	}
	v.Dev = suffix != "" && describeSuffix(suffix)
	return v
}

// describeSuffix reports whether a suffix is one "git describe" appends -
// "3-gc0ffee", "dirty", "3-gc0ffee-dirty" - rather than a pre-release marker
// such as "rc1".
func describeSuffix(s string) bool {
	s = strings.TrimSuffix(s, "-dirty")
	if s == "" || s == "dirty" {
		return true
	}
	count, hash, ok := strings.Cut(s, "-")
	if !ok {
		return false
	}
	if _, err := strconv.Atoi(count); err != nil {
		return false
	}
	return strings.HasPrefix(hash, "g") && len(hash) > 1
}

// Compare orders two versions: -1 when v is the older, 0 when they are the
// same build, +1 when v is the newer.
func (v Version) Compare(o Version) int {
	for _, pair := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		switch {
		case pair[0] < pair[1]:
			return -1
		case pair[0] > pair[1]:
			return 1
		}
	}
	switch {
	case v.Suffix == o.Suffix:
		return 0
	case v.Dev && o.Suffix == "":
		return 1 // built from commits made after that tag
	case o.Dev && v.Suffix == "":
		return -1
	case v.Suffix == "":
		return 1 // a release supersedes its own pre-releases
	case o.Suffix == "":
		return -1
	}
	return strings.Compare(v.Suffix, o.Suffix)
}

// String renders the version the way the tags spell it.
func (v Version) String() string {
	if !v.OK {
		return ""
	}
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Suffix != "" {
		s += "-" + v.Suffix
	}
	return s
}

// --- releases --------------------------------------------------------------

// Release is one published release and the assets it carries.
type Release struct {
	Tag       string
	Published time.Time
	Page      string
	Assets    map[string]string // asset name -> download URL
}

// Version is the release's tag, parsed.
func (r *Release) Version() Version { return Parse(r.Tag) }

// AssetNames lists the names that could hold a platform's binary, the most
// specific first. Releases so far publish "mystrelay" and "mystrelay.exe" for
// amd64 alone; a platform-tagged name is preferred so that publishing other
// architectures later cannot hand this one the wrong file.
func AssetNames(goos, goarch string) []string {
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	names := []string{fmt.Sprintf("mystrelay_%s_%s%s", goos, goarch, ext)}
	if goarch == "amd64" {
		names = append(names, "mystrelay"+ext)
	}
	return names
}

// Binary returns the asset holding the binary for a platform.
func (r *Release) Binary(goos, goarch string) (name, downloadURL string, err error) {
	for _, want := range AssetNames(goos, goarch) {
		if u, ok := r.Assets[want]; ok {
			return want, u, nil
		}
	}
	return "", "", fmt.Errorf("release %s publishes no binary for %s/%s (it carries: %s)",
		r.Tag, goos, goarch, r.assetList())
}

// Sums returns the URL of the release's digest list. A release without one is
// refused rather than installed unchecked.
func (r *Release) Sums() (string, error) {
	u, ok := r.Assets[SumsAsset]
	if !ok {
		return "", fmt.Errorf("release %s publishes no %s, so a download from it cannot be checked", r.Tag, SumsAsset)
	}
	return u, nil
}

func (r *Release) assetList() string {
	names := make([]string, 0, len(r.Assets))
	for name := range r.Assets {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "nothing"
	}
	return strings.Join(names, ", ")
}

// ParseSums reads the "<sha256>  <name>" lines of a SHA256SUMS file. Anything
// else in the file is ignored.
func ParseSums(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		// sha256sum marks a binary-mode digest with a * before the name.
		out[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return out
}

// --- the release API -------------------------------------------------------

// Client reads releases from a GitHub repository and downloads their assets.
type Client struct {
	HTTP      *http.Client // required
	Repo      string       // owner/name; DefaultRepo when empty
	UserAgent string       // GitHub refuses requests carrying none
	API       string       // base URL; GitHub's when empty
}

// Latest returns the newest published release. Drafts and pre-releases are not
// among them: GitHub's "latest" excludes both.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	return c.release(ctx, "/repos/"+c.repo()+"/releases/latest")
}

// AtTag returns one named release, pre-release or not.
func (c *Client) AtTag(ctx context.Context, tag string) (*Release, error) {
	return c.release(ctx, "/repos/"+c.repo()+"/releases/tags/"+url.PathEscape(tag))
}

func (c *Client) release(ctx context.Context, path string) (*Release, error) {
	body, err := c.get(ctx, c.api()+path, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("the release API answered with something that is not a release: %w", err)
	}
	if doc.TagName == "" {
		return nil, fmt.Errorf("the release API answered without a tag name")
	}
	rel := &Release{Tag: doc.TagName, Published: doc.PublishedAt, Page: doc.HTMLURL, Assets: map[string]string{}}
	for _, a := range doc.Assets {
		rel.Assets[a.Name] = a.URL
	}
	return rel, nil
}

// Fetch reads a small asset - the digest list - into memory.
func (c *Client) Fetch(ctx context.Context, assetURL string) ([]byte, error) {
	return c.get(ctx, assetURL, "")
}

// Download fetches assetURL into dir and returns the path it was written to,
// having checked that what arrived hashes to want. A file that does not match
// is deleted, not kept for inspection: nothing unverified is left lying next to
// the binary it would have replaced.
func (c *Client) Download(ctx context.Context, assetURL, dir, want string) (string, error) {
	resp, err := c.do(ctx, assetURL, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	tmp, err := os.CreateTemp(dir, "mystrelay-download-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()

	sum := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(resp.Body, maxAsset))
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(name)
		if copyErr != nil {
			return "", fmt.Errorf("download %s: %w", assetURL, copyErr)
		}
		return "", closeErr
	}

	if got := hex.EncodeToString(sum.Sum(nil)); !strings.EqualFold(got, want) {
		os.Remove(name)
		return "", fmt.Errorf("the download does not match the checksum published with the release\n  expected %s\n  got      %s\nNothing was installed", want, got)
	}
	if err := os.Chmod(name, 0o755); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func (c *Client) get(ctx context.Context, target, accept string) ([]byte, error) {
	resp, err := c.do(ctx, target, accept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (c *Client) do(ctx context.Context, target, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, statusError(target, resp)
	}
	return resp, nil
}

func statusError(target string, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("nothing published at %s", target)
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return fmt.Errorf("GitHub is rate limiting this address - unauthenticated requests are capped at 60 an hour%s", rateLimitReset(resp))
		}
	}
	return fmt.Errorf("%s returned HTTP %d", target, resp.StatusCode)
}

func rateLimitReset(resp *http.Response) string {
	secs, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || secs == 0 {
		return ""
	}
	return ", until " + time.Unix(secs, 0).Local().Format("15:04")
}

func (c *Client) repo() string {
	if c.Repo == "" {
		return DefaultRepo
	}
	return c.Repo
}

func (c *Client) api() string {
	if c.API == "" {
		return defaultAPI
	}
	return strings.TrimSuffix(c.API, "/")
}

func (c *Client) userAgent() string {
	if c.UserAgent == "" {
		return "myst-relay"
	}
	return c.UserAgent
}

// --- installing ------------------------------------------------------------

// Running is the path of the executing binary, with symlinks resolved so an
// update replaces the file rather than a link pointing at it.
func Running() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		return real, nil
	}
	return exe, nil
}

// OldPath is where Replace moves the binary it supersedes.
func OldPath(target string) string { return target + ".old" }

// Replace puts src at target, which is normally the running executable.
//
// Windows will not unlink a running image but will happily rename it, so the
// superseded binary is moved aside rather than deleted - this process is still
// executing it. CleanOld removes it at the next start. Everywhere else the
// rename is atomic and leaves nothing behind.
func Replace(src, target string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(src, target)
	}
	old := OldPath(target)
	_ = os.Remove(old)
	if err := os.Rename(target, old); err != nil {
		return err
	}
	if err := os.Rename(src, target); err != nil {
		// Put the working binary back rather than leaving nothing there.
		_ = os.Rename(old, target)
		return err
	}
	return nil
}

// CleanOld deletes a superseded binary left behind by an earlier update. It
// does nothing until the process that was running it has exited, which is why
// it is called at startup rather than at the end of an update.
func CleanOld(target string) {
	_ = os.Remove(OldPath(target))
}
