//go:build windows

package main

import (
	"encoding/xml"
	"fmt"
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
	var trigger, principal, restart, action string

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

	default:
		return fmt.Errorf("scope must be logon or boot, got %q", o.Scope)
	}

	doc := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <URI>\%s</URI>
    <Description>myst-relay - fail-closed proxy relay over the Mysterium network</Description>
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
</Task>`, taskName, trigger, principal, restart, action)

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
// The relay is a console program, so launching it directly at logon would leave
// a console window on screen for as long as it runs. PowerShell starts it
// hidden instead, which is the least surprising thing for something meant to
// sit in the background. The config path is passed as one already-quoted
// argument because Start-Process joins -ArgumentList with spaces and quotes
// nothing, which would split a path like C:\Users\Jean Dupont\...
func hiddenLaunch(o autostartOptions) string {
	ps := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	command := fmt.Sprintf(
		`Start-Process -FilePath %s -ArgumentList ('run -config ' + [char]34 + %s + [char]34) -WindowStyle Hidden`,
		ps(o.Executable), ps(o.ConfigPath))
	return execAction(powershellPath(),
		fmt.Sprintf(`-NoProfile -NonInteractive -WindowStyle Hidden -Command "%s"`, command))
}

// directLaunch is the boot action: SYSTEM runs in session 0, where there is no
// window to hide, so the task runs the relay itself and owns it.
func directLaunch(o autostartOptions) string {
	return execAction(o.Executable, `run -config "`+o.ConfigPath+`"`)
}

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
	return strings.TrimSpace(string(out)), nil
}

// restartHint is what to type to put a newly installed binary into service.
// The scheduler owns the process when autostart is registered, so stopping the
// task and starting it again is the whole of it.
func restartHint() string {
	if taskExists() {
		return "  schtasks /end /tn " + taskName + "\n  schtasks /run /tn " + taskName
	}
	return "  stop it where it is running, then: " + exeName() + " run"
}
