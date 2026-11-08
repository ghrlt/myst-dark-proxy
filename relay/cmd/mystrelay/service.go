package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// serviceInfo is what the platform's service manager knows about the relay's
// registration - the systemd unit on Linux, the scheduled task on Windows.
type serviceInfo struct {
	Installed bool
	// Via names the registration, for messages: which unit, which task.
	Via string
	// Active is whether the manager says the service is running, and
	// KnowsActive whether that means anything: a Windows logon task launches
	// the icon and exits, so its state says nothing about the relay.
	Active      bool
	KnowsActive bool
	// Start asks the manager to start the service.
	Start func() error
	// Logs is what to type to see why it did not come up.
	Logs string
}

func cmdStartService(args []string) error {
	fs := newFlagSet("startservice")
	cfgPath := fs.String("config", "", "path to config.env")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for the relay to answer once the service is started")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	svc := lookupService()
	if cfg.LogFile != "" {
		svc.Logs = logsHint(cfg.LogFile)
	}
	return startService(svc, newRelayClient(cfg), *wait, os.Stdout)
}

// startService starts the installed service and returns once the relay it
// runs is answering, so that "started" means the ports are there to be used,
// not merely that a service manager accepted a request.
func startService(svc serviceInfo, client *relayClient, wait time.Duration, out io.Writer) error {
	if !svc.Installed {
		return fmt.Errorf("no service is installed for the relay. Install one with:\n  %s autostart install\n"+
			"or run it in the foreground with:\n  %s run", exeName(), exeName())
	}

	ask := func() (*statusReport, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return client.status(ctx)
	}

	if rep, err := ask(); err == nil {
		// Something already answers on the relay's port. If it is not the
		// service, starting the service would only give it a relay that cannot
		// bind its ports and a manager restarting it in a loop.
		if svc.KnowsActive && !svc.Active {
			return fmt.Errorf("a relay is already running, but not as the service (%s): it holds the ports the service needs.\n"+
				"Stop it first with \"%s stop\", then run this again", svc.Via, exeName())
		}
		fmt.Fprintf(out, "the relay is already running - %s\n", describeTunnel(rep))
		return nil
	}

	if err := svc.Start(); err != nil {
		return err
	}

	deadline := time.Now().Add(wait)
	for {
		rep, err := ask()
		if err == nil {
			fmt.Fprintf(out, "relay started (%s) - %s\n", svc.Via, describeTunnel(rep))
			return nil
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("the service was started (%s) but the relay has not answered on %s after %s", svc.Via, client.base, wait)
			if svc.Logs != "" {
				msg += ".\nSee what it said with:\n  " + svc.Logs
			}
			return errors.New(msg)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func describeTunnel(rep *statusReport) string {
	s := "tunnel " + rep.Tunnel.State
	if rep.Tunnel.Detail != "" {
		s += " (" + rep.Tunnel.Detail + ")"
	}
	return s
}

// startCommand is what to type to start the relay: through its service when
// one is installed, since a relay started by hand next to an installed service
// is the one combination that fights over the ports later.
func startCommand() string {
	if lookupService().Installed {
		return exeName() + " startservice"
	}
	return exeName() + " run"
}
