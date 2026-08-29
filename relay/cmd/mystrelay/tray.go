//go:build windows || linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/systray"
)

// The tray icon is the desktop face of the relay.
//
// It is a client, not the service: it asks the running relay what it is doing
// over the same status port everything else uses, and asks it to do things over
// the control endpoints. Nothing about the kill switch, the tunnel or the
// frontends is reachable from here except through those endpoints, so an icon
// that crashes, is closed, or was never started changes nothing about whether
// traffic is being carried.
//
// It will also start a relay that is not running, and supervise the one it
// started, because that is what makes one icon at sign-in the whole of a
// desktop install. A relay the machine's own supervisor already runs - the
// Windows scheduled task, the systemd user unit - is left to its owner, and the
// icon merely watches it.
//
// Everything here is the same on both desktops. What differs - how a child is
// detached, and how the machine's own supervisor is asked to start or stop the
// relay - is behind the four functions at the bottom of this file, implemented
// in tray_windows.go and tray_linux.go.

const (
	// pollEvery is how often the relay is asked what it is doing. It is a
	// loopback request against a server that answers from memory, so this is
	// cheap enough to keep the icon honest without being a busy loop.
	pollEvery = 5 * time.Second
	// startupGrace is how long the tray waits for a relay somebody else is
	// starting before concluding that nobody is, and starting one itself.
	startupGrace = 20 * time.Second
	// stopGrace is how long a relay asked to stop is given to do it properly.
	// Stopping properly matters more here than elsewhere: the relay is what
	// shuts the myst node down, and one killed outright leaves the node behind
	// holding the port its replacement needs.
	stopGrace = 25 * time.Second
)

func cmdTray(args []string) error {
	fs := newFlagSet("tray")
	cfgPath := fs.String("config", "", "path to config.env")
	attach := fs.Bool("attach", false, "only watch a relay somebody else runs; never start or stop one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *cfgPath
	if path == "" {
		path = config.ConfigPath()
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}

	app := &trayApp{
		cfg:     cfg,
		cfgPath: path,
		client:  newRelayClient(cfg),
		attach:  *attach,
	}

	// Started from the scheduler or a shortcut, this process is handed a
	// console nobody wants to look at. Started from a terminal, it shares one
	// that is not ours to close; DetachConsole knows the difference.
	systray.DetachConsole()

	return systray.Run(systray.Options{
		Title:   "myst-relay",
		Tooltip: "myst-relay - starting",
		Colour:  systray.Grey,
		Menu:    app.menu,
		Ready:   app.watch,
	})
}

type trayApp struct {
	cfg     *config.Config
	cfgPath string
	client  *relayClient
	attach  bool

	mu       sync.Mutex
	icon     *systray.Tray
	report   *statusReport // nil when nothing answered the last poll
	child    *exec.Cmd     // the relay this tray started, if it started one
	stopping bool          // the child's exit is one we asked for
	muted    int           // depth of "the operator is doing this on purpose"
	backoff  time.Duration
}

// --- the loop --------------------------------------------------------------

// watch keeps the icon in step with the relay for as long as the tray runs.
func (a *trayApp) watch(icon *systray.Tray) {
	a.mu.Lock()
	a.icon = icon
	a.mu.Unlock()

	// A relay already answering belongs to whoever started it, and only silence
	// means this tray should start one. The wait matters at sign-in: a boot
	// task's relay may still be opening its ports, and a second one started on
	// top of it would fail on the ports the first already holds.
	if a.poll() == nil && !a.attach && !a.waitForRelay(startupGrace) {
		// Not deferred: this function does not return, and a mute left on
		// would silence every balloon for the rest of the session.
		resume := a.deliberately()
		if err := a.startRelay(); err != nil {
			icon.Notify("myst-relay", "Could not start the relay: "+err.Error(), true)
		}
		a.poll()
		resume()
	}

	for {
		time.Sleep(pollEvery)
		a.poll()
	}
}

// poll asks the relay how it is and repaints the icon. It returns the report,
// or nil when nothing answered.
func (a *trayApp) poll() *statusReport {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	rep, err := a.client.status(ctx)
	if err != nil {
		rep = nil
	}

	a.mu.Lock()
	previous := a.report
	// The first look is the state of the world, not a change in it.
	settled := a.report != nil || a.child != nil
	quiet := a.muted > 0
	a.report = rep
	icon := a.icon
	a.mu.Unlock()

	if icon != nil {
		colour, tip := iconFor(rep)
		icon.Set(colour, tip)
		if settled && !quiet {
			announce(icon, previous, rep)
		}
	}
	return rep
}

// deliberately marks the start of something the operator asked for, and returns
// the function that marks its end. Balloons are held back in between: being
// told that what you just clicked has happened is noise, and the click already
// gets an answer of its own.
func (a *trayApp) deliberately() func() {
	a.mu.Lock()
	a.muted++
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.muted--
		a.mu.Unlock()
	}
}

// iconFor maps what the relay says onto the colour and hover text.
func iconFor(rep *statusReport) (systray.Colour, string) {
	if rep == nil {
		return systray.Grey, "myst-relay - not running"
	}
	switch {
	case rep.Healthy:
		where := strings.TrimSpace(rep.Tunnel.Country + " " + rep.Tunnel.IPType)
		tip := "myst-relay - carrying traffic"
		if rep.Tunnel.ExitIP != "" {
			tip += " via " + rep.Tunnel.ExitIP
		}
		if where != "" {
			tip += " (" + where + ")"
		}
		return systray.Green, tip
	case rep.Tunnel.State == "idle":
		return systray.Blue, "myst-relay - resting: no session, the next client opens one"
	case rep.Ready, rep.Tunnel.State == "connecting":
		return systray.Amber, "myst-relay - building a session"
	}
	reason := rep.KillSwitch.Reason
	if reason == "" {
		reason = rep.Tunnel.State
	}
	return systray.Red, "myst-relay - refusing traffic: " + trunc(reason, 80)
}

// announce raises a balloon for the two changes worth interrupting somebody
// for: traffic stopping, and traffic being possible again. Every other
// transition - a session opening, an exit rotating - is visible in the icon and
// is not worth a notification.
func announce(icon *systray.Tray, was, now *statusReport) {
	switch {
	case was == nil && now != nil:
		icon.Notify("myst-relay", "The relay is running again.", false)
	case was != nil && now == nil:
		icon.Notify("myst-relay", "The relay has stopped. Nothing will reach the proxy ports.", true)
	case was != nil && now != nil && was.Ready && !now.Ready:
		reason := now.KillSwitch.Reason
		if reason == "" {
			reason = "the tunnel is " + now.Tunnel.State
		}
		icon.Notify("Traffic stopped", "The kill switch is closed: "+trunc(reason, 180), true)
	case was != nil && now != nil && !was.Ready && now.Ready:
		icon.Notify("myst-relay", "Carrying traffic again.", false)
	}
}

// --- the menu --------------------------------------------------------------

func (a *trayApp) menu() []systray.Item {
	a.mu.Lock()
	rep, owned := a.report, a.child != nil
	a.mu.Unlock()

	items := []systray.Item{systray.Label(headlineFor(rep))}
	if rep != nil && rep.Healthy {
		items = append(items, systray.Label("   "+exitLine(rep)))
	}
	items = append(items, systray.Separator(),
		systray.Item{Label: "Status...", Bold: true, Do: a.showStatus},
		systray.Item{Label: "Usage...", Do: a.showUsage},
		systray.Item{Label: "Balance...", Do: a.showBalance},
		systray.Separator())

	if rep == nil {
		if !a.attach {
			items = append(items, systray.Item{Label: "Start relay", Do: a.doStart}, systray.Separator())
		}
		return append(items, systray.Item{Label: "Quit tray icon", Do: a.doQuit})
	}

	change := systray.Item{Label: "Change exit node", Do: a.doChangeExit}
	restart := systray.Item{Label: "Restart relay", Do: a.doRestart}
	stop := systray.Item{Label: "Stop relay", Do: a.doStop}
	if !rep.Control {
		// The relay has no control secret, so it takes no instructions. Show
		// what is missing rather than offering buttons that answer 404.
		reason := " (this relay takes no instructions)"
		change.Disabled, restart.Disabled, stop.Disabled = true, true, true
		change.Label += reason
		restart.Label += reason
		stop.Label += reason
	}
	if a.attach && !owned {
		stop.Label = "Stop relay (started elsewhere)"
	}

	items = append(items, change, systray.Separator(), restart, stop, systray.Separator())

	// Quitting is two items rather than one and a question. Which one is the
	// surprise depends on who started the relay, so both are spelled out and
	// neither is a default.
	if owned {
		items = append(items,
			systray.Item{Label: "Quit icon, leave the relay running", Do: a.doQuit},
			systray.Item{Label: "Quit icon and stop the relay", Do: a.doQuitAndStop})
	} else {
		items = append(items, systray.Item{Label: "Quit tray icon", Do: a.doQuit})
	}
	return items
}

func headlineFor(rep *statusReport) string {
	switch {
	case rep == nil:
		return "myst-relay - not running"
	case rep.Healthy:
		return "myst-relay - carrying traffic"
	case rep.Tunnel.State == "idle":
		return "myst-relay - resting between clients"
	case rep.Ready:
		return "myst-relay - building a session"
	}
	return "myst-relay - refusing traffic"
}

func exitLine(rep *statusReport) string {
	parts := []string{}
	for _, s := range []string{rep.Tunnel.ExitIP, rep.Tunnel.Country, rep.Tunnel.IPType} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "  -  ")
}

// --- what the menu items do ------------------------------------------------

func (a *trayApp) showStatus() {
	rep := a.fresh()
	if rep == nil {
		systray.Warn("myst-relay", "The relay is not answering on "+a.cfg.HealthListen+
			".\n\nIt is not running, or it is still starting up.")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", headlineFor(rep))
	fmt.Fprintf(&b, "tunnel         %s\n", rep.Tunnel.State)
	fmt.Fprintf(&b, "sessions       %s\n", sessionLine(rep))
	if rep.Tunnel.ProviderID != "" {
		fmt.Fprintf(&b, "exit           %s   %s %s\n", orDash(rep.Tunnel.ExitIP), orDash(rep.Tunnel.Country), orDash(rep.Tunnel.IPType))
		fmt.Fprintf(&b, "provider       %s\n", rep.Tunnel.ProviderID)
	}
	if rep.Tunnel.Detail != "" {
		fmt.Fprintf(&b, "detail         %s\n", rep.Tunnel.Detail)
	}
	fmt.Fprintf(&b, "kill switch    %s\n", killSwitchLine(rep))
	if rep.KillSwitch.Reason != "" {
		fmt.Fprintf(&b, "               %s\n", rep.KillSwitch.Reason)
	}
	fmt.Fprintf(&b, "uptime         %s\n", rep.Uptime)
	if rep.Version != "" {
		fmt.Fprintf(&b, "version        %s\n", rep.Version)
	}
	if len(rep.Endpoints) > 0 {
		b.WriteString("\nPoint your applications at:\n")
		for _, e := range rep.Endpoints {
			fmt.Fprintf(&b, "  %-8s %s\n", e.Name, e.Addr)
		}
	}
	systray.Show("myst-relay status", b.String())
}

func killSwitchLine(rep *statusReport) string {
	state := "closed"
	switch {
	case rep.KillSwitch.Open:
		state = "open"
	case rep.KillSwitch.Armed:
		state = "armed, waiting for a session"
	}
	if rep.KillSwitch.Strict {
		state += ", strict"
	}
	return fmt.Sprintf("%s;  %d admitted, %d refused, %d open now",
		state, rep.KillSwitch.Admitted, rep.KillSwitch.Refused, rep.KillSwitch.Active)
}

func (a *trayApp) showUsage() {
	rep := a.fresh()
	if rep == nil {
		systray.Warn("myst-relay", "The relay is not answering, so there is nothing to report.")
		return
	}
	if rep.Usage == nil {
		systray.Show("myst-relay usage", "This relay does not keep usage figures.")
		return
	}
	u := rep.Usage
	var b strings.Builder
	// A session can be open with the gate shut, so what is asked here is
	// whether one exists, not whether it is admitting traffic.
	if rep.Tunnel.State == "up" {
		b.WriteString("This session\n")
		fmt.Fprintf(&b, "  carried      %s up, %s down\n", humanBytes(u.Session.BytesSent), humanBytes(u.Session.BytesReceived))
		fmt.Fprintf(&b, "  cost         %s MYST\n", u.Session.MYST)
		fmt.Fprintf(&b, "  open for     %s\n\n", orDash(u.Session.Duration))
	} else {
		b.WriteString("No session is open right now.\n\n")
	}
	b.WriteString("Since the relay started\n")
	fmt.Fprintf(&b, "  carried      %s up, %s down\n", humanBytes(u.Total.BytesSent), humanBytes(u.Total.BytesReceived))
	fmt.Fprintf(&b, "  cost         %s MYST\n", u.Total.MYST)
	fmt.Fprintf(&b, "  sessions     %d, %s connected\n", u.Sessions, orDash(u.Total.Duration))
	fmt.Fprintf(&b, "  streams      %d admitted, %d refused\n", rep.KillSwitch.Admitted, rep.KillSwitch.Refused)
	fmt.Fprintf(&b, "  uptime       %s\n", rep.Uptime)
	b.WriteString("\nThe figures come from the myst node, which counts what the exit\n" +
		"charges for. Totals reset when the relay restarts.")
	systray.Show("myst-relay usage", b.String())
}

func (a *trayApp) showBalance() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bal, err := a.client.balance(ctx)
	if err != nil {
		systray.Warn("MYST balance", "Could not read the balance.\n\n"+err.Error()+
			"\n\nOnly the myst node knows it, so this needs the relay to be running.")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s MYST\n\n", bal.MYST)
	fmt.Fprintf(&b, "identity       %s\n", bal.Identity)
	fmt.Fprintf(&b, "registration   %s\n", bal.Registration)
	if bal.Channel != "" {
		fmt.Fprintf(&b, "channel        %s\n", bal.Channel)
	}
	fmt.Fprintf(&b, "\nRead from the node at %s.\n", bal.At.Local().Format("15:04:05"))
	b.WriteString("Top it up in the Mysterium app; this service spends an identity,\nit does not fund one.")
	systray.Show("MYST balance", b.String())
}

func (a *trayApp) doChangeExit() {
	// Traffic stopping is what an exit change is; being warned about it is not
	// news to whoever just asked for it.
	defer a.deliberately()()

	icon := a.tray()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	changed, detail, err := a.client.rotate(ctx, "asked from the tray")
	if err != nil {
		systray.Warn("Change exit node", "The relay would not change exit.\n\n"+err.Error())
		return
	}
	if !changed {
		if icon != nil {
			icon.Notify("myst-relay", detail, false)
		}
		return
	}
	if icon == nil {
		return
	}
	icon.Notify("Changing exit node", "Leaving this provider; another one is being picked.", false)
	rep := awaitExit(ctx, a.client)
	if rep == nil {
		icon.Notify("myst-relay", "No session yet. The next client opens one on a new exit.", false)
		return
	}
	icon.Notify("New exit node", exitLine(rep), false)
}

func (a *trayApp) doStart() {
	defer a.deliberately()()
	if err := a.startRelay(); err != nil {
		systray.Warn("Start relay", "Could not start the relay.\n\n"+err.Error())
		return
	}
	a.poll()
}

func (a *trayApp) doStop() {
	defer a.deliberately()()
	if err := a.stopRelay(); err != nil {
		systray.Warn("Stop relay", err.Error())
	}
	a.poll()
}

func (a *trayApp) doRestart() {
	defer a.deliberately()()
	if err := a.stopRelay(); err != nil {
		systray.Warn("Restart relay", "Could not stop the running relay.\n\n"+err.Error())
		return
	}
	if err := a.startRelay(); err != nil {
		systray.Warn("Restart relay", "The relay was stopped but would not start again.\n\n"+err.Error())
		return
	}
	if icon := a.tray(); icon != nil {
		icon.Notify("myst-relay", "Restarted. The first connection after this takes a moment.", false)
	}
	a.poll()
}

// doQuit takes the icon down and leaves the relay running.
func (a *trayApp) doQuit() {
	if icon := a.tray(); icon != nil {
		icon.Quit()
	}
}

// doQuitAndStop takes both away. It is a menu item of its own rather than a
// question asked on the way out: a proxy left running with no icon to show it
// is a surprise, and so is one stopped by a click that only said "quit".
func (a *trayApp) doQuitAndStop() {
	defer a.deliberately()()
	if err := a.stopRelay(); err != nil {
		systray.Warn("Quit myst-relay", err.Error())
		return
	}
	if icon := a.tray(); icon != nil {
		icon.Quit()
	}
}

// fresh polls now rather than showing what the last tick happened to see: a
// menu item opened by hand deserves an answer from this moment.
func (a *trayApp) fresh() *statusReport { return a.poll() }

func (a *trayApp) tray() *systray.Tray {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.icon
}

// --- starting and stopping -------------------------------------------------

// startRelay puts a relay back in service.
//
// A scheduled task is asked first when one exists: it is what owns the relay on
// a machine set up to start automatically, and on a boot-scope install it is
// the only thing that can start it as the account it is meant to run as. When
// there is no task, or the scheduler refuses, the tray starts it itself and
// supervises what it started.
func (a *trayApp) startRelay() error {
	if a.probe() != nil {
		return nil
	}
	// The machine's own supervisor is the right lever whenever it is the thing
	// that runs the relay here: it starts it as the account it is meant to run
	// as, and it will put it back if it dies. Where the relay is not its job -
	// a desktop install where it starts this tray instead - starting it would
	// put a second icon on the panel rather than a relay behind the first.
	if supervisorRunsRelay() {
		if err := supervisorStart(); err == nil && a.waitForRelay(45*time.Second) {
			return nil
		}
	}
	return a.spawn()
}

// spawn starts the relay as a child of the tray.
func (a *trayApp) spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "run", "-config", a.cfgPath)
	detachChild(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}

	a.mu.Lock()
	a.child, a.stopping = cmd, false
	a.mu.Unlock()

	go a.supervise(cmd)
	if !a.waitForRelay(60 * time.Second) {
		return fmt.Errorf("the relay was started but is not answering on %s; see the log in %s",
			a.cfg.HealthListen, a.cfg.LogFile)
	}
	return nil
}

// supervise waits for a relay this tray started and puts it back when it dies
// on its own. A relay that fails immediately and repeatedly is misconfigured
// rather than unlucky, so the wait between attempts grows, and after enough of
// them the tray says so and stops trying.
func (a *trayApp) supervise(cmd *exec.Cmd) {
	const giveUpAfter = time.Minute

	started := time.Now()
	err := cmd.Wait()

	a.mu.Lock()
	asked, current := a.stopping, a.child == cmd
	if current {
		a.child = nil
	}
	if time.Since(started) > giveUpAfter {
		a.backoff = 0
	}
	wait := a.backoff
	if wait == 0 {
		wait = 5 * time.Second
	}
	a.backoff = min(wait*2, 5*time.Minute)
	icon := a.icon
	a.mu.Unlock()

	// It stopped because we asked, or it was replaced; either way it is not
	// this goroutine's business any more.
	if asked || !current {
		return
	}

	if icon != nil {
		icon.Notify("myst-relay", fmt.Sprintf("The relay exited (%v). Starting it again in %s.", err, wait), true)
	}
	time.Sleep(wait)

	a.mu.Lock()
	replaced := a.child != nil
	a.mu.Unlock()
	if replaced {
		return
	}
	if err := a.spawn(); err != nil && icon != nil {
		icon.Notify("myst-relay", "Could not restart the relay: "+err.Error(), true)
	}
}

// stopRelay asks the relay to shut down and waits for it to go quiet.
//
// Asking is not optional politeness here. The relay is what stops the myst
// node, so one killed outright leaves the node running and holding the API port
// its replacement needs; termination is only the fallback for a relay that will
// not answer.
func (a *trayApp) stopRelay() error {
	a.mu.Lock()
	child := a.child
	a.stopping = true
	a.mu.Unlock()

	// Where the machine's supervisor owns the relay, asking the relay to stop
	// is not enough: a clean exit is exactly what "restart it whenever it
	// stops" is for, and it would be back within seconds. The supervisor has to
	// be the one told.
	if child == nil && supervisorRunsRelay() {
		if err := supervisorStop(); err == nil && a.waitForSilence(stopGrace) {
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), stopGrace)
	defer cancel()

	asked := a.client.stop(ctx)
	if asked == nil && a.waitForSilence(stopGrace) {
		a.forget(child)
		return nil
	}
	if child == nil {
		if asked != nil {
			return fmt.Errorf("the relay would not stop: %w", asked)
		}
		return fmt.Errorf("the relay was asked to stop but is still answering on %s", a.cfg.HealthListen)
	}
	// Ours, and it did not go quietly.
	_ = child.Process.Kill()
	a.waitForSilence(10 * time.Second)
	a.forget(child)
	return nil
}

func (a *trayApp) forget(child *exec.Cmd) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if child != nil && a.child == child {
		a.child = nil
	}
}

// waitForRelay blocks until the relay answers, or the wait runs out.
func (a *trayApp) waitForRelay(within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if a.probe() != nil {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

// waitForSilence blocks until nothing answers on the status port. The port
// going quiet is the proof that the relay stopped, and it is what anything
// starting in its place has to wait for anyway.
func (a *trayApp) waitForSilence(within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if a.probe() == nil {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// --- the platform seam -----------------------------------------------------
//
// Four things differ between the two desktops, and only these four:
//
//	detachChild        how a spawned relay is given no window and its own life
//	supervisorRunsRelay  whether the machine's own supervisor runs the relay here
//	supervisorStart    ask that supervisor to start it
//	supervisorStop     ask it to stop, and stay stopped
//
// They are implemented in tray_windows.go against the scheduled task and in
// tray_linux.go against the systemd user unit.

// probe asks once whether anything is answering on the status port, without
// touching the icon. It is the question every wait below is built out of.
func (a *trayApp) probe() *statusReport {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rep, err := a.client.status(ctx)
	if err != nil {
		return nil
	}
	return rep
}
