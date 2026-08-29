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
	// Tray asks a logon registration to put the status icon in the
	// notification area as well.
	//
	// The two desktops arrange that differently. On Windows the scheduled task
	// starts the icon and the icon starts the relay, so one icon at sign-in is
	// the whole of a desktop install. On Linux the systemd user unit keeps the
	// relay - it is the better supervisor - and the icon is a separate session
	// entry that attaches to it. Either way it is meaningless at boot scope:
	// nothing has a desktop to draw on before anyone signs in.
	Tray bool
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
	tray := fs.Bool("tray", true, "logon scope: also start the notification-area status icon")
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
			Tray:       *tray,
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
		return fmt.Errorf("usage: %s autostart install|remove|status [-scope logon|boot] [-tray=false]", exeName())
	}
}
