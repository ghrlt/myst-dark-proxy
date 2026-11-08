//go:build windows

package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
)

func defaultConfigPath() string  { return config.ConfigPath() }
func appDirOf(cfg string) string { return filepath.Dir(cfg) }

// The task's description says what it starts. Task Scheduler shows it in its
// own list, where "myst-relay" alone tells an owner nothing - and it is also
// how a registration is dated: one that says none of these was written before
// there was a status icon to start, and legacyDescription is what every build
// up to v0.5.0 wrote instead.
const (
	descIconLogon  = "myst-relay - the status icon at sign-in, and the relay behind it"
	descRelayLogon = "myst-relay - the relay at sign-in, without the status icon"
	descRelayBoot  = "myst-relay - the relay at startup, before anyone signs in"

	legacyDescription = "myst-relay - fail-closed proxy relay over the Mysterium network"
)

// installAutostart registers a Scheduled Task from an XML definition rather
// than from schtasks flags.
//
// The flag form of a logon task, "/sc onlogon" with no account named, puts no
// user on the trigger, and Windows reads that as "at the logon of anyone". Such
// a task acts on other people's sessions, so only an administrator may register
// it - which is why the logon scope used to fail with "access is denied" from
// an ordinary prompt. Naming the account keeps the task yours, and registering
// your own task needs nothing but being that account.
//
// The XML is also where the settings a long-running background task needs live:
// no three-day execution limit, and no refusing to start because a laptop
// happens to be on battery.
func installAutostart(o autostartOptions) error {
	// Both launch paths below redirect the process they start into this file,
	// so a panic that would otherwise vanish into a hidden or session-0 process
	// with no console at all lands somewhere. The redirect target has to exist
	// before Start-Process does, or it fails to create the process at all.
	if err := os.MkdirAll(filepath.Join(o.AppDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("create logs directory: %w", err)
	}

	var trigger, principal, restart, action, description string

	switch o.Scope {
	case "logon":
		who, err := currentAccount()
		if err != nil {
			return err
		}
		trigger = fmt.Sprintf(`    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>`, esc(who))
		principal = fmt.Sprintf(`    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>`, esc(who))
		action = hiddenLaunch(o)
		description = descRelayLogon
		if o.Tray {
			description = descIconLogon
		}

	case "boot":
		trigger = `    <BootTrigger>
      <Enabled>true</Enabled>
      <Delay>PT30S</Delay>
    </BootTrigger>`
		principal = `    <Principal id="Author">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>`
		// A boot task owns the relay process itself, so the scheduler can put
		// it back if it dies - the same intent as Restart=always in the systemd
		// unit the other platforms get.
		restart = `
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>99</Count>
    </RestartOnFailure>`
		action = directLaunch(o)
		description = descRelayBoot

	default:
		return fmt.Errorf("scope must be logon or boot, got %q", o.Scope)
	}

	doc := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <URI>\%s</URI>
    <Description>%s</Description>
  </RegistrationInfo>
  <Triggers>
%s
  </Triggers>
  <Principals>
%s
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>%s
  </Settings>
  <Actions Context="Author">
%s
  </Actions>
</Task>`, taskName, esc(description), trigger, principal, restart, action)

	path, err := writeTaskXML(doc)
	if err != nil {
		return err
	}
	defer os.Remove(path)

	out, err := exec.Command("schtasks", "/create", "/tn", taskName, "/xml", path, "/f").CombinedOutput()
	if err != nil {
		hint := "\nA task registered for boot runs as the system account, so creating or\nreplacing it needs an Administrator prompt."
		if o.Scope == "logon" && !taskExists() {
			hint = ""
		}
		return fmt.Errorf("schtasks: %w\n%s%s", err, strings.TrimSpace(string(out)), hint)
	}
	return nil
}

// hiddenLaunch is the logon action.
//
// What it starts is the tray icon by default, and the tray starts the relay: a
// desktop install is then one thing in the notification area with the service
// behind it, rather than a background process with no way to see or steer it.
// "autostart install -tray=false" goes back to launching the relay alone.
//
// Either way it goes through PowerShell. Both are console programs, so
// launching one directly at logon would leave a console window on screen for as
// long as it runs; Start-Process starts it hidden instead, which is the least
// surprising thing for something meant to sit in the background. The config
// path is passed as one already-quoted argument because Start-Process joins
// -ArgumentList with spaces and quotes nothing, which would split a path like
// C:\Users\Jean Dupont\...
func hiddenLaunch(o autostartOptions) string {
	verb := "run"
	if o.Tray {
		verb = "tray"
	}
	// Only stderr is redirected: that is where a Go panic's stack trace is
	// written, and Start-Process refuses to redirect stdout and stderr to the
	// same file.
	command := fmt.Sprintf(
		`Start-Process -FilePath %s -ArgumentList ('%s -config ' + [char]34 + %s + [char]34) -WindowStyle Hidden -RedirectStandardError %s`,
		ps(o.Executable), verb, ps(o.ConfigPath), ps(crashLogPath(o.AppDir)))
	return execAction(powershellPath(),
		fmt.Sprintf(`-NoProfile -NonInteractive -WindowStyle Hidden -Command "%s"`, command))
}

// directLaunch is the boot action: SYSTEM runs in session 0, where there is no
// window to hide and, since Task Scheduler gives a launched process no console
// of its own either, nowhere a panic's stack trace would land. Going through
// Start-Process here too, rather than the plain <Exec> a foreground process
// would use, is what lets that output be redirected; -Wait and propagating its
// exit code keep the task's own RestartOnFailure watching the relay's actual
// exit rather than the wrapper's.
func directLaunch(o autostartOptions) string {
	command := fmt.Sprintf(
		`$p = Start-Process -FilePath %s -ArgumentList ('run -config ' + [char]34 + %s + [char]34) -RedirectStandardError %s -NoNewWindow -PassThru -Wait; exit $p.ExitCode`,
		ps(o.Executable), ps(o.ConfigPath), ps(crashLogPath(o.AppDir)))
	return execAction(powershellPath(),
		fmt.Sprintf(`-NoProfile -NonInteractive -WindowStyle Hidden -Command "%s"`, command))
}

// ps quotes a string as a single-quoted PowerShell literal.
func ps(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// crashLogPath is where a launch redirects the relay's own stderr. Nothing
// writes there on purpose - slog has its own file - so it should stay empty;
// it only fills when the relay panics or the Go runtime itself dies, output a
// hidden or session-0 process would otherwise lose outright.
func crashLogPath(appDir string) string { return filepath.Join(appDir, "logs", "crash.log") }

func execAction(command, args string) string {
	return fmt.Sprintf(`    <Exec>
      <Command>%s</Command>
      <Arguments>%s</Arguments>
    </Exec>`, esc(command), esc(args))
}

// currentAccount identifies the signed-in account for the task's trigger and
// principal. The SID is preferred: it survives a rename and does not depend on
// how the machine spells its own name.
func currentAccount() (string, error) {
	if u, err := user.Current(); err == nil {
		if u.Uid != "" {
			return u.Uid, nil
		}
		if u.Username != "" {
			return u.Username, nil
		}
	}
	name := os.Getenv("USERNAME")
	if name == "" {
		return "", fmt.Errorf("cannot tell which account to register the task for")
	}
	if domain := os.Getenv("USERDOMAIN"); domain != "" {
		return domain + `\` + name, nil
	}
	return name, nil
}

func powershellPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "powershell.exe"
}

// writeTaskXML spills the definition to a file for schtasks to read. Task
// Scheduler exports UTF-16, and reads it back most reliably.
func writeTaskXML(doc string) (string, error) {
	f, err := os.CreateTemp("", "myst-relay-task-*.xml")
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, 0, 2+2*len(doc))
	buf = append(buf, 0xFF, 0xFE) // UTF-16 LE byte order mark
	for _, u := range utf16.Encode([]rune(doc)) {
		buf = append(buf, byte(u), byte(u>>8))
	}
	if _, err := f.Write(buf); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// taskExists asks the scheduler rather than reading its message, which is
// translated into the language Windows was installed in.
func taskExists() bool {
	return exec.Command("schtasks", "/query", "/tn", taskName).Run() == nil
}

// registration is the part of a registered task worth reading back.
type registration struct {
	Description string   `xml:"RegistrationInfo>Description"`
	LogonUsers  []string `xml:"Triggers>LogonTrigger>UserId"`
	Arguments   []string `xml:"Actions>Exec>Arguments"`
}

// readTask returns what is registered under our name, if anything is.
func readTask() (registration, bool) {
	out, err := exec.Command("schtasks", "/query", "/tn", taskName, "/xml", "ONELINE").Output()
	if err != nil {
		return registration{}, false
	}
	dec := xml.NewDecoder(strings.NewReader(decodeTaskXML(out)))
	// The declaration says UTF-16 because that is what schtasks exports;
	// decodeTaskXML has already turned the bytes into text, so the reader is
	// handed straight back rather than converted a second time.
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	var reg registration
	if err := dec.Decode(&reg); err != nil {
		return registration{}, false
	}
	return reg, true
}

// decodeTaskXML turns what schtasks printed into text. It exports UTF-16 with a
// byte order mark; older builds and some locales print plain bytes instead, so
// both are accepted.
func decodeTaskXML(out []byte) string {
	if len(out) < 2 || out[0] != 0xFF || out[1] != 0xFE {
		return string(out)
	}
	units := make([]uint16, 0, (len(out)-2)/2)
	for i := 2; i+1 < len(out); i += 2 {
		units = append(units, uint16(out[i])|uint16(out[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

// adoptStatusIcon puts the notification-area icon into a registration made
// before there was one.
//
// Every build up to v0.5.0 registered a sign-in task that launches the relay
// directly, because there was no icon to launch instead. Replacing the binary
// leaves that task exactly as it was, so a machine that has been running the
// relay since before the icon existed would sign in to no icon for ever -
// which is the whole of what its owner sees of this feature, and not something
// they can be expected to know to go and fix by hand. So the relay fixes it
// itself, once, on a start where it can see that this is what happened.
//
// It is deliberately narrow, because rewriting the machine's own registration
// is not a thing to do on a guess. Only a sign-in task is touched: a boot task
// needs an administrator and has no desktop to draw on anyway. Only one whose
// action names this very executable, so a relay run by hand from a download
// folder cannot point the registration at itself. And only one whose
// description shows it was written before the icon existed, so that
// "autostart install -tray=false" - which writes a description saying exactly
// that - is a choice that stands. The one gap is a task registered by v0.5.0
// itself with -tray=false: it describes itself the old way, and so gains an
// icon it was told to do without, once.
func adoptStatusIcon(log *slog.Logger) {
	reg, ok := readTask()
	if !ok || len(reg.LogonUsers) == 0 || reg.Description != legacyDescription {
		return
	}
	action := strings.Join(reg.Arguments, " ")
	if !strings.Contains(action, "run -config") || strings.Contains(action, "tray -config") {
		return
	}
	// The launcher is written from two quoted strings and reads back as three:
	// the executable, the verb, and the config path it is given.
	quoted := psQuoted(action)
	if len(quoted) < 3 {
		return
	}
	exe, cfgPath := quoted[0], quoted[len(quoted)-1]
	if self, err := os.Executable(); err != nil || !samePath(self, exe) {
		return
	}
	if err := installAutostart(autostartOptions{
		Scope:      "logon",
		Executable: exe,
		ConfigPath: cfgPath,
		AppDir:     appDirOf(cfgPath),
		Tray:       true,
	}); err != nil {
		log.Warn("could not add the status icon to the sign-in task", "task", taskName, "error", err)
		return
	}
	log.Info("the sign-in task now starts the status icon, and the icon starts the relay",
		"task", taskName, "icon_from", "the next sign-in, or "+exeName()+" tray")
}

// psQuoted returns the single-quoted strings in a PowerShell command line, in
// order, undoing the doubling that quotes a quote. It is the reverse of how
// hiddenLaunch writes the paths into the action, and so the way to read back
// what an already registered task was told to start.
func psQuoted(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if s[i] != '\'' {
			i++
			continue
		}
		var b strings.Builder
		for i++; i < len(s); i++ {
			if s[i] != '\'' {
				b.WriteByte(s[i])
				continue
			}
			if i+1 >= len(s) || s[i+1] != '\'' {
				i++
				break
			}
			b.WriteByte('\'')
			i++
		}
		out = append(out, b.String())
	}
	return out
}

// samePath compares two Windows paths as Windows does.
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func removeAutostart() error {
	if !taskExists() {
		return nil
	}
	out, err := exec.Command("schtasks", "/delete", "/tn", taskName, "/f").CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks: %w\n%s\nA task registered for boot belongs to the system account; removing it\nneeds an Administrator prompt.",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

func autostartStatus() (string, error) {
	out, err := exec.Command("schtasks", "/query", "/tn", taskName, "/fo", "list").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("not installed. Register it with: %s autostart install -scope logon", exeName()), nil
	}
	// What it starts is the question people actually have, and the task listing
	// answers it only in a command line long enough to hide the answer.
	lead := ""
	if reg, ok := readTask(); ok {
		switch reg.Description {
		case descIconLogon:
			lead = "starts: the status icon, and the icon starts the relay\n\n"
		case descRelayLogon:
			lead = fmt.Sprintf("starts: the relay alone, with no status icon\n"+
				"        put one there with \"%s autostart install\"\n\n", exeName())
		case descRelayBoot:
			lead = "starts: the relay as the system account, before anyone signs in\n\n"
		case legacyDescription:
			lead = fmt.Sprintf("starts: the relay alone - this task was registered before the status\n"+
				"        icon existed. \"%s autostart install\" adds it, and so does\n"+
				"        the next start of the relay itself.\n\n", exeName())
		}
	}
	return lead + strings.TrimSpace(string(out)), nil
}

// restartHint is what to type to put a newly installed binary into service.
//
// The relay is asked to stop rather than the task being ended: at logon scope
// the task's own process is the launcher, which exited seconds after sign-in,
// so ending the task would leave the relay running. Stopping through the relay
// itself works whatever started it.
func restartHint() string {
	if taskExists() {
		return "  " + exeName() + " stop\n  schtasks /run /tn " + taskName +
			"\n\nOr use the tray icon: right-click it and choose Restart relay."
	}
	return "  " + exeName() + " stop\n  " + exeName() + " run"
}

// lookupService finds the scheduled task "autostart install" registered.
// Running it does what signing in does: start the icon, which starts the
// relay - or, for a boot task, the relay itself.
func lookupService() serviceInfo {
	if !taskExists() {
		return serviceInfo{}
	}
	return serviceInfo{
		Installed: true,
		Via:       "scheduled task " + taskName,
		// The task's own state is the launcher's, which exits once the relay
		// is up; only the relay answering says anything.
		KnowsActive: false,
		Start: func() error {
			out, err := exec.Command("schtasks", "/run", "/tn", taskName).CombinedOutput()
			if err != nil {
				return fmt.Errorf("schtasks: %w\n%s\nA task registered for boot belongs to the system account; starting it\nneeds an Administrator prompt.",
					err, strings.TrimSpace(string(out)))
			}
			return nil
		},
	}
}

func logsHint(path string) string {
	return "powershell -NoProfile -Command \"Get-Content -Tail 50 '" + path + "'\""
}
