package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"unicode/utf16"
)

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// LaunchdPlist is the LaunchAgent definition (S7: RunAtLoad + KeepAlive, installed and removed with
// launchctl bootstrap/bootout, no admin).
func LaunchdPlist(s Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + Label + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(s.Exe) + `</string>
`)
	for _, a := range s.Args {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>ProcessType</key><string>Background</string>
`)
	if s.WorkDir != "" {
		b.WriteString("  <key>WorkingDirectory</key><string>" + xmlEscape(s.WorkDir) + "</string>\n")
	}
	if s.LogPath != "" {
		b.WriteString("  <key>StandardOutPath</key><string>" + xmlEscape(s.LogPath) + "</string>\n")
		b.WriteString("  <key>StandardErrorPath</key><string>" + xmlEscape(s.LogPath) + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// systemdQuote quotes one word for an ExecStart line. systemd expands % and $ itself, so they are
// doubled.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

// SystemdUnit is the user unit. The log is appended to a file (systemd 240+); older versions fall
// back to the journal through `journalctl --user -u noskill`.
func SystemdUnit(s Spec) string {
	var exec []string
	exec = append(exec, systemdQuote(s.Exe))
	for _, a := range s.Args {
		exec = append(exec, systemdQuote(a))
	}
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=noskill: local model skills, OpenAI and Ollama compatible server\nAfter=network.target\n\n[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + strings.Join(exec, " ") + "\n")
	if s.WorkDir != "" {
		b.WriteString("WorkingDirectory=" + strings.ReplaceAll(s.WorkDir, " ", `\x20`) + "\n")
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\n")
	if s.LogPath != "" {
		b.WriteString("StandardOutput=append:" + s.LogPath + "\nStandardError=append:" + s.LogPath + "\n")
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

// WindowsTaskXML is a Task Scheduler definition that starts the server when the user logs on,
// runs as that user with limited rights, and restarts it if it stops. schtasks wants UTF-16.
func WindowsTaskXML(s Spec, user string) []byte {
	cmdline := `"` + s.Exe + `"`
	for _, a := range s.Args {
		if strings.ContainsAny(a, ` "`) {
			a = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
		cmdline += " " + a
	}
	// Run through cmd so the log redirection works. cmd /c drops the first and last quote when a
	// line has more than two, which breaks paths with spaces, so the whole command gets one more
	// pair of quotes around it.
	inner := cmdline
	if s.LogPath != "" {
		inner += ` >> "` + s.LogPath + `" 2>&1`
	}
	args := `/c "` + inner + `"`
	doc := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>noskill background server</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + xmlEscape(user) + `</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>` + xmlEscape(user) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>5</Count></RestartOnFailure>
    <Hidden>true</Hidden>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author"><Exec><Command>cmd.exe</Command><Arguments>` + xmlEscape(args) + `</Arguments><WorkingDirectory>` + xmlEscape(s.WorkDir) + `</WorkingDirectory></Exec></Actions>
</Task>
`
	u := utf16.Encode([]rune(doc))
	out := []byte{0xFF, 0xFE} // BOM
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

// XDGAutostart is the fallback on Linux without systemd --user.
func XDGAutostart(s Spec) string {
	var exec []string
	exec = append(exec, desktopQuote(s.Exe))
	for _, a := range s.Args {
		exec = append(exec, desktopQuote(a))
	}
	return fmt.Sprintf("[Desktop Entry]\nType=Application\nName=noskill\nComment=noskill background server\nExec=%s\nTerminal=false\nX-GNOME-Autostart-enabled=true\n", strings.Join(exec, " "))
}

func desktopQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\$`<>~|&;*?#()") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
	return `"` + r.Replace(s) + `"`
}
