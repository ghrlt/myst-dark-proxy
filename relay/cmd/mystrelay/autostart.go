package main

import (
	"fmt"
	"os"
)

// autostartOptions describes an autostart registration.
type autostartOptions struct {
	// Scope is "logon" (as the signed-in user) or "boot" (before sign-in).
	Scope      string
	Executable string
	ConfigPath string
	AppDir     string
}

// taskName is the identifier used by whichever scheduler the platform has.
const taskName = "myst-relay"

func cmdAutostart(args []string) error {
	action := "status"
	rest := args
	if len(args) > 0 && !isFlag(args[0]) {
		action, rest = args[0], args[1:]
	}

	fs := newFlagSet("autostart")
	scope := fs.String("scope", "logon", "logon | boot")
	cfgPath := fs.String("config", "", "path to config.env")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	switch action {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		path := *cfgPath
		if path == "" {
			path = defaultConfigPath()
		}
		if err := installAutostart(autostartOptions{
			Scope:      *scope,
			Executable: exe,
			ConfigPath: path,
			AppDir:     appDirOf(path),
		}); err != nil {
			return err
		}
		fmt.Printf("autostart installed (%s)\n", *scope)
		return nil

	case "remove", "uninstall":
		if err := removeAutostart(); err != nil {
			return err
		}
		fmt.Println("autostart removed")
		return nil

	case "status":
		s, err := autostartStatus()
		if err != nil {
			return err
		}
		fmt.Println(s)
		return nil

	default:
		return fmt.Errorf("usage: %s autostart install|remove|status [-scope logon|boot]", exeName())
	}
}
