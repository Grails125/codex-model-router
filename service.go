package main

import (
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

func windowsTaskXML(binaryPath, dataDir string) ([]byte, error) {
	var escape = func(value string) string {
		var buffer strings.Builder
		_ = xml.EscapeText(&buffer, []byte(value))
		return buffer.String()
	}
	userID := strings.TrimSpace(os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME"))
	if userID == `\` {
		return nil, errors.New("unable to determine Windows user")
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Loopback Responses routing for OpenAI and DeepSeek in one Codex model picker</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>serve --data-dir &quot;%s&quot;</Arguments>
      <WorkingDirectory>%s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`, escape(userID), escape(userID), escape(binaryPath), escape(dataDir), escape(dataDir))
	return []byte(plist), nil
}

func encodeUTF16LE(value []byte) []byte {
	runes := utf16.Encode([]rune(string(value)))
	result := make([]byte, 2+len(runes)*2)
	result[0], result[1] = 0xff, 0xfe
	for index, unit := range runes {
		binary.LittleEndian.PutUint16(result[2+index*2:], unit)
	}
	return result
}

func macLaunchAgentPlist(binaryPath, dataDir string) []byte {
	escape := func(value string) string {
		var buffer strings.Builder
		_ = xml.EscapeText(&buffer, []byte(value))
		return buffer.String()
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>serve</string>
    <string>--data-dir</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, escape(serviceLabel), escape(binaryPath), escape(dataDir),
		escape(filepath.Join(dataDir, "service.log")),
		escape(filepath.Join(dataDir, "service.log"))))
}

func installService(binaryPath, dataDir string) error {
	switch runtime.GOOS {
	case "windows":
		exists, matches, err := windowsTaskMatches(binaryPath, dataDir)
		if err != nil {
			return err
		}
		if exists {
			if !matches {
				return errors.New("existing Codex Model Router task belongs to another installation")
			}
			return run("schtasks.exe", "/Run", "/TN", "Codex Model Router")
		}
		taskXML, err := windowsTaskXML(binaryPath, dataDir)
		if err != nil {
			return err
		}
		taskPath := filepath.Join(dataDir, "windows-task.xml")
		if err := atomicWrite(taskPath, encodeUTF16LE(taskXML), 0o600); err != nil {
			return err
		}
		if err := run("schtasks.exe", "/Create", "/F", "/TN", "Codex Model Router", "/XML", taskPath); err != nil {
			return err
		}
		return run("schtasks.exe", "/Run", "/TN", "Codex Model Router")
	case "darwin":
		_ = stopService()
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		agents := filepath.Join(home, "Library", "LaunchAgents")
		if err := ensureDir(agents); err != nil {
			return err
		}
		plistPath := filepath.Join(agents, serviceLabel+".plist")
		if err := atomicWrite(plistPath, macLaunchAgentPlist(binaryPath, dataDir), 0o600); err != nil {
			return err
		}
		uidBytes, err := exec.Command("id", "-u").Output()
		if err != nil {
			return errors.New("unable to determine macOS user id")
		}
		domain := "gui/" + strings.TrimSpace(string(uidBytes))
		if err := run("launchctl", "bootstrap", domain, plistPath); err != nil {
			return run("launchctl", "load", "-w", plistPath)
		}
		return nil
	default:
		return fmt.Errorf("background service installation is not implemented for %s; use --no-service", runtime.GOOS)
	}
}

// Only stop an existing Windows task when its executable and data directory
// match. The legacy Python task and arbitrary listeners are left untouched.
func stopInstalledService(binaryPath, dataDir string) error {
	if runtime.GOOS != "windows" {
		return stopService()
	}
	exists, matches, err := windowsTaskMatches(binaryPath, dataDir)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !matches {
		return errors.New("existing Codex Model Router task belongs to another installation")
	}
	// /End is asynchronous. replaceBinary waits for Windows to release its image.
	_ = run("schtasks.exe", "/End", "/TN", "Codex Model Router")
	return nil
}

func windowsTaskMatches(binaryPath, dataDir string) (bool, bool, error) {
	raw, err := exec.Command("schtasks.exe", "/Query", "/TN", "Codex Model Router", "/XML").Output()
	if err != nil {
		return false, false, nil
	}
	matches, err := taskXMLMatches(raw, binaryPath, dataDir)
	return true, matches, err
}

func taskXMLMatches(raw []byte, binaryPath, dataDir string) (bool, error) {
	if len(raw) >= 2 && raw[0] == 0xff && raw[1] == 0xfe {
		units := make([]uint16, 0, (len(raw)-2)/2)
		for i := 2; i+1 < len(raw); i += 2 {
			units = append(units, binary.LittleEndian.Uint16(raw[i:i+2]))
		}
		raw = []byte(string(utf16.Decode(units)))
	}
	var task struct {
		Actions struct {
			Exec []struct {
				Command          string
				Arguments        string
				WorkingDirectory string
			}
		}
	}
	decoder := xml.NewDecoder(strings.NewReader(string(raw)))
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	if err := decoder.Decode(&task); err != nil {
		return false, errors.New("unable to validate existing router task")
	}
	if len(task.Actions.Exec) != 1 {
		return false, nil
	}
	action := task.Actions.Exec[0]
	return strings.EqualFold(filepath.Clean(action.Command), filepath.Clean(binaryPath)) &&
		action.Arguments == `serve --data-dir "`+dataDir+`"`, nil
}

func macBootoutArguments(uid string) []string {
	// bootout accepts one service-target; a label alone is not a plist path.
	return []string{"bootout", "gui/" + strings.TrimSpace(uid) + "/" + serviceLabel}
}

func stopService() error {
	switch runtime.GOOS {
	case "windows":
		_ = run("schtasks.exe", "/End", "/TN", "Codex Model Router")
		_ = run("schtasks.exe", "/Delete", "/F", "/TN", "Codex Model Router")
		return nil
	case "darwin":
		uidBytes, err := exec.Command("id", "-u").Output()
		if err == nil {
			_ = run("launchctl", macBootoutArguments(string(uidBytes))...)
		}
		home, _ := os.UserHomeDir()
		plist := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
		_ = os.Remove(plist)
		return nil
	default:
		return nil
	}
}
