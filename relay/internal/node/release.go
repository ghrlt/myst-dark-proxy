// Package node fetches and supervises a Mysterium node binary.
//
// The Docker deployment runs the node as its own container and the relay just
// attaches to it. On a desktop - Windows in particular - there is no compose
// file to do that, so the relay owns the node process itself: it downloads the
// official release, verifies it, starts it in proxy mode, and keeps it alive.
package node

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultVersion is the node release this relay is built and tested against.
const DefaultVersion = "1.39.9"

// Archive describes one downloadable node build.
type Archive struct {
	URL    string
	SHA256 string // of the archive as published
	Member string // path of the node binary inside the archive
	Kind   string // zip | targz
}

// BinaryName is the on-disk name the node binary is given.
func (a Archive) BinaryName() string {
	if strings.HasSuffix(a.Member, ".exe") {
		return "myst.exe"
	}
	return "myst"
}

// Checksums are recorded here rather than fetched, so a tampered or swapped
// release is caught at install time instead of trusted blindly. They were
// computed from the published archives.
var releases = map[string]map[string]Archive{
	"1.39.9": {
		"windows/amd64": {
			URL:    "https://github.com/mysteriumnetwork/node/releases/download/1.39.9/myst_windows_amd64.zip",
			SHA256: "00e82637f77b028ed5a1fadc7bdb5da5380fbf61a0018fd47eb669e883a37b61",
			Member: "myst.exe",
			Kind:   "zip",
		},
		"linux/amd64": {
			URL:    "https://github.com/mysteriumnetwork/node/releases/download/1.39.9/myst_linux_amd64.tar.gz",
			SHA256: "a6aadca289c4b3c3f3b66f2e24c101e4bd60b9c4ea8f7b5ee550810b99d4008a",
			Member: "./myst",
			Kind:   "targz",
		},
	},
	"1.39.5": {
		"windows/amd64": {
			URL:    "https://github.com/mysteriumnetwork/node/releases/download/1.39.5/myst_windows_amd64.zip",
			SHA256: "1fb71a249f90c403124ef8cfae116d9128d915d945374a08594550d897a03cc6",
			Member: "myst.exe",
			Kind:   "zip",
		},
		"linux/amd64": {
			URL:    "https://github.com/mysteriumnetwork/node/releases/download/1.39.5/myst_linux_amd64.tar.gz",
			SHA256: "818f813808e235760e5049c6f74b97ab1cce36b26f148a56ddea10271ffb8d87",
			Member: "./myst",
			Kind:   "targz",
		},
	},
}

// ArchiveFor returns the release for a platform, or an error listing what is
// available.
func ArchiveFor(version, platform string) (Archive, error) {
	byPlatform, ok := releases[version]
	if !ok {
		return Archive{}, fmt.Errorf("no recorded checksums for node version %q (known: %s)", version, keys(releases))
	}
	a, ok := byPlatform[platform]
	if !ok {
		return Archive{}, fmt.Errorf("node %s is not packaged for %s (available: %s)", version, platform, keys(byPlatform))
	}
	return a, nil
}

// Platform is the current GOOS/GOARCH pair.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Ensure returns the path to a verified node binary in dir, downloading it if
// it is not already there. The download is checked against the recorded digest
// before anything is extracted.
func Ensure(ctx context.Context, dir, version string, log *slog.Logger) (string, error) {
	a, err := ArchiveFor(version, Platform())
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	binary := filepath.Join(dir, a.BinaryName())
	stamp := filepath.Join(dir, ".node-"+version+"-"+a.SHA256[:12])
	if fileExists(binary) && fileExists(stamp) {
		return binary, nil
	}

	log.Info("downloading Mysterium node", "version", version, "platform", Platform(), "url", a.URL)
	tmp, err := download(ctx, a, dir)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)

	log.Info("verifying and unpacking node")
	if err := extract(tmp, a, binary); err != nil {
		return "", err
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return "", err
	}
	// The stamp is written last: an interrupted install is retried rather than
	// mistaken for a good one.
	if err := os.WriteFile(stamp, []byte(a.URL+"\n"), 0o644); err != nil {
		return "", err
	}
	cleanStamps(dir, filepath.Base(stamp))
	log.Info("node ready", "path", binary)
	return binary, nil
}

func download(ctx context.Context, a Archive, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download node: %s returned HTTP %d", a.URL, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(dir, "node-download-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()

	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, sum), resp.Body)
	closeErr := tmp.Close()
	if err != nil {
		os.Remove(name)
		return "", fmt.Errorf("download node: %w", err)
	}
	if closeErr != nil {
		os.Remove(name)
		return "", closeErr
	}

	got := hex.EncodeToString(sum.Sum(nil))
	if got != a.SHA256 {
		os.Remove(name)
		return "", fmt.Errorf("downloaded node does not match its recorded checksum\n  expected %s\n  got      %s\nRefusing to run it", a.SHA256, got)
	}
	return name, nil
}

func extract(archivePath string, a Archive, dest string) error {
	switch a.Kind {
	case "zip":
		return extractZip(archivePath, a.Member, dest)
	case "targz":
		return extractTarGz(archivePath, a.Member, dest)
	default:
		return fmt.Errorf("unknown archive kind %q", a.Kind)
	}
}

func extractZip(path, member, dest string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		if !sameMember(f.Name, member) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return writeFile(dest, rc)
	}
	return fmt.Errorf("%s not found inside %s", member, filepath.Base(path))
}

func extractTarGz(path, member, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || !sameMember(h.Name, member) {
			continue
		}
		return writeFile(dest, tr)
	}
	return fmt.Errorf("%s not found inside %s", member, filepath.Base(path))
}

// sameMember compares archive paths tolerantly: releases have shipped the same
// file as "myst", "./myst" and "myst.exe" across builds.
func sameMember(name, member string) bool {
	trim := func(s string) string {
		return strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(s), "./"), "/")
	}
	return trim(name) == trim(member)
}

func writeFile(dest string, src io.Reader) error {
	tmp := dest + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Windows will not rename over a running executable; remove first.
	_ = os.Remove(dest)
	return os.Rename(tmp, dest)
}

// cleanStamps drops markers from superseded versions so the directory does not
// accumulate them.
func cleanStamps(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".node-") && e.Name() != keep {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func keys[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ", ")
}
