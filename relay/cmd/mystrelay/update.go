package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/node"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/selfupdate"
)

// updateAvailable is the exit status "update -check" ends with when a newer
// release exists, so a scheduled check can act on it without reading the text.
const updateAvailable = 10

// cmdUpdate compares this binary with the published releases and, unless asked
// only to look, replaces it with a newer one.
//
// The check happens when it is run and at no other time: a relay carrying
// traffic never asks GitHub anything, so nothing announces this machine on a
// schedule. What it costs is that being out of date is not noticed by itself -
// "update -check" is cheap to put on a timer if that trade is the wrong way
// round for you.
func cmdUpdate(args []string) error {
	fs := newFlagSet("update")
	var (
		checkOnly = fs.Bool("check", false, "report what is available and install nothing (exit 10 when there is an update)")
		force     = fs.Bool("force", false, "install even when the release is not newer than this build")
		to        = fs.String("to", "", "install this release rather than the newest one (e.g. v0.3.0)")
		repo      = fs.String("repo", selfupdate.DefaultRepo, "GitHub repository to take releases from")
		viaRelay  = fs.Bool("via-relay", false, "fetch through the running relay, so the request leaves from the exit node")
		cfgPath   = fs.String("config", "", "path to config.env")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	target, err := selfupdate.Running()
	if err != nil {
		return err
	}
	// A binary superseded by an earlier update on Windows. This process is not
	// the one running it, so it can go now.
	selfupdate.CleanOld(target)

	if !*checkOnly && inContainer() {
		return fmt.Errorf("this binary is the one baked into the container image, on a read-only filesystem.\n" +
			"Update the image instead:\n  make update        # pull a newer node image and rebuild the relay\nSee README, \"Keeping it current\"")
	}

	client, via, err := releaseClient(*cfgPath, *viaRelay)
	if err != nil {
		return err
	}
	releases := &selfupdate.Client{HTTP: client, Repo: *repo, UserAgent: "myst-relay/" + version}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	fmt.Printf("\nmyst-relay update\n\n")
	fmt.Printf("  installed  %s  (%s)\n", version, node.Platform())

	var rel *selfupdate.Release
	if *to != "" {
		rel, err = releases.AtTag(ctx, *to)
	} else {
		rel, err = releases.Latest(ctx)
	}
	if err != nil {
		return fmt.Errorf("ask %s for releases%s: %w", *repo, via, err)
	}

	published := ""
	if !rel.Published.IsZero() {
		published = ", published " + rel.Published.Local().Format("2006-01-02")
	}
	label := "latest"
	if *to != "" {
		label = "asked for"
	}
	fmt.Printf("  %-9s  %s%s\n", label, rel.Tag, published)
	fmt.Printf("             %s\n\n", rel.Page)

	current := selfupdate.Parse(version)
	newer := !current.OK || current.Compare(rel.Version()) < 0
	switch {
	case !current.OK:
		fmt.Println("  this build carries no version, so there is nothing to compare it against")
	case newer:
		fmt.Println("  an update is available")
	case current.Compare(rel.Version()) == 0:
		fmt.Println("  already up to date")
	default:
		fmt.Println("  this build is newer than that release")
	}

	if *checkOnly {
		if newer {
			fmt.Printf("\n  install it with \"%s update\"\n\n", exeName())
			return statusExit(updateAvailable)
		}
		fmt.Println()
		return nil
	}
	if !newer && !*force && *to == "" {
		fmt.Printf("\n  nothing to do - \"%s update -force\" reinstalls it anyway\n\n", exeName())
		return nil
	}

	return installRelease(ctx, releases, rel, target, *cfgPath, via)
}

// installRelease downloads a release and puts it in place of the running binary.
func installRelease(ctx context.Context, releases *selfupdate.Client, rel *selfupdate.Release, target, cfgPath, via string) error {
	name, assetURL, err := rel.Binary(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	sumsURL, err := rel.Sums()
	if err != nil {
		return err
	}
	sums, err := releases.Fetch(ctx, sumsURL)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", selfupdate.SumsAsset, err)
	}
	want := selfupdate.ParseSums(sums)[name]
	if want == "" {
		return fmt.Errorf("%s lists no digest for %s, so the download cannot be checked", selfupdate.SumsAsset, name)
	}

	fmt.Printf("\ndownloading %s %s%s\n", name, rel.Tag, via)
	dir, staged := installDir(target)
	tmp, err := releases.Download(ctx, assetURL, dir, want)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(tmp)
		}
	}()
	fmt.Printf("  checksum   matches the release (%s...)\n", want[:16])

	// Run the new binary once, before it replaces anything. A file that will
	// not execute - the wrong architecture, a release built for something else
	// - is caught here rather than after it has taken the place of a relay
	// that worked.
	got, nodeVersion, err := probe(ctx, tmp)
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run, so it was not installed: %w", err)
	}
	fmt.Printf("  runs       %s, node %s\n", got, orDash(nodeVersion))
	if v := selfupdate.Parse(got); v.OK && rel.Version().OK && v.Compare(rel.Version()) != 0 {
		fmt.Printf("  ! it reports %s though the release is tagged %s\n", got, rel.Tag)
	}

	if staged {
		keep = true
		waiting := filepath.Join(filepath.Dir(tmp), "mystrelay-"+rel.Tag+selfupdate.Ext())
		if err := os.Rename(tmp, waiting); err == nil {
			tmp = waiting
		}
		return fmt.Errorf("%s cannot be written by this account, so the new binary was not installed\n"+
			"  it is verified and waiting at %s\n"+
			"  put it in place with:\n    %s",
			filepath.Dir(target), tmp, elevatedInstall(tmp, target))
	}
	if err := selfupdate.Replace(tmp, target); err != nil {
		keep = true
		return fmt.Errorf("replace %s: %w\n  the new binary is verified and waiting at %s", target, err, tmp)
	}
	fmt.Printf("installed    %s -> %s at %s\n", version, rel.Tag, target)

	afterInstall(cfgPath, rel.Tag, nodeVersion)
	return nil
}

// elevatedInstall is how to finish an install that needed a privilege this
// account has not got. It differs by platform, and an instruction telling a
// Windows owner to use sudo is no instruction at all.
func elevatedInstall(src, target string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("copy /Y %q %q\n  from a terminal opened with \"Run as administrator\"", src, target)
	}
	return fmt.Sprintf("sudo install -m 0755 %s %s", src, target)
}

// afterInstall says what the new binary does not pick up by itself.
func afterInstall(cfgPath, tag, nodeVersion string) {
	path := cfgPath
	if path == "" {
		path = config.ConfigPath()
	}

	// The node version follows the binary unless the config file pins one.
	// Setups written before that was true pinned it explicitly, which would
	// hold a new relay on the node its predecessor shipped with.
	if pin := pinnedNodeVersion(path); pin != "" && nodeVersion != "" && pin != nodeVersion {
		fmt.Printf("\n  note  %s pins MYST_NODE_VERSION=%s, and %s ships node %s.\n"+
			"        Delete that line to follow the version this binary is tested against.\n",
			path, pin, tag, nodeVersion)
	}

	// Only the file on disk has changed; whatever is running is still the old
	// one, holding its ports.
	if cfg, err := loadConfig(cfgPath); err == nil {
		if _, err := fetchStatus(cfg.HealthListen); err == nil {
			fmt.Printf("\nThe relay running now is still %s. Restart it to pick this up:\n%s\n", version, restartHint())
		}
	}
	fmt.Println()
}

// releaseClient builds the HTTP client the check and the download use, and
// describes where it goes.
//
// -via-relay sends both through the relay's own SOCKS5 port, so asking about
// updates leaves from the exit node like any other client's traffic and GitHub
// learns that someone is looking rather than which machine. It costs a session:
// the relay has to be up before it can be updated.
func releaseClient(cfgPath string, viaRelay bool) (*http.Client, string, error) {
	if !viaRelay {
		return &http.Client{
			Timeout:   15 * time.Minute,
			Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		}, "", nil
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, "", err
	}
	if cfg.SocksListen == "" {
		return nil, "", fmt.Errorf("-via-relay needs the SOCKS5 frontend, and RELAY_SOCKS_LISTEN is empty")
	}
	client := socksClient(cfg.SocksListen, cfg.Auth.Username, cfg.Auth.Password)
	client.Timeout = 15 * time.Minute
	return client, " through the relay on " + cfg.SocksListen, nil
}

// probe asks a binary what it is. The version command prints one line:
//
//	myst-relay v0.4.0 (node 1.39.5, windows/amd64)
func probe(ctx context.Context, path string) (version, nodeVersion string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	line := strings.TrimSpace(string(out))
	if err != nil {
		return "", "", fmt.Errorf("%w: %s", err, trunc(line, 120))
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "myst-relay" {
		return "", "", fmt.Errorf("it printed %q rather than a version", trunc(line, 60))
	}
	return fields[1], after(line, "(node ", ",)"), nil
}

// after returns what follows start, up to the first of the stop characters.
func after(s, start, stop string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	if j := strings.IndexAny(rest, stop); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// installDir picks where the download lands. The folder holding the binary is
// what makes the swap atomic - a rename within one filesystem - but an install
// under a root-owned prefix is not writable by the person running this, and
// there the file is staged in the temp directory instead and left for them to
// move with the privileges that needs.
func installDir(target string) (dir string, staged bool) {
	dir = filepath.Dir(target)
	f, err := os.CreateTemp(dir, ".mystrelay-writable-*")
	if err != nil {
		return os.TempDir(), true
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return dir, false
}

// pinnedNodeVersion reports a node version pinned in a config file, if any.
func pinnedNodeVersion(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == "MYST_NODE_VERSION" {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// inContainer reports whether this is the binary baked into the container
// image, where an update means a new image rather than a new file.
func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return os.Getenv("container") != ""
}
