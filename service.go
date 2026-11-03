package main

import (
	"bytes"
	"debug/pe"
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

func windowsConsoleBinaryPath(binaryPath string) string {
	return strings.TrimSuffix(binaryPath, ".exe") + "-cli.exe"
}

// Keep the task's existing executable path so an upgrade never needs permission
// to rewrite its registration. Its installed image uses the GUI subsystem;
// the sibling console copy supports diagnostics and interactive key changes.
func installBackgroundBinary(sourceExecutable, installedBinary string, noService bool) error {
	if runtime.GOOS != "windows" || noService {
		return nil
	}
	raw, err := os.ReadFile(sourceExecutable)
	if err != nil {
		return err
	}
	background, err := windowsGUIBinary(raw)
	if err != nil {
		return err
	}
	console, err := windowsSubsystemBinary(raw, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI)
	if err != nil {
		return err
	}
	if err := replaceBinaryIfChanged(windowsConsoleBinaryPath(installedBinary), console); err != nil {
		return err
	}
	return replaceBinaryIfChanged(installedBinary, background)
}

func replaceBinaryIfChanged(path string, raw []byte) error {
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, raw) {
		// The console companion can itself be running this reinstall command.
		return nil
	}
	return replaceBinary(path, raw)
}

func windowsGUIBinary(raw []byte) ([]byte, error) {
	return windowsSubsystemBinary(raw, pe.IMAGE_SUBSYSTEM_WINDOWS_GUI)
}

func windowsSubsystemBinary(raw []byte, subsystem uint16) ([]byte, error) {
	if subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI && subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_CUI {
		return nil, errors.New("unsupported target Windows executable subsystem")
	}
	image, err := pe.NewFile(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("router executable is not a valid Windows PE image")
	}
	defer image.Close()
	if image.Machine != pe.IMAGE_FILE_MACHINE_AMD64 && image.Machine != pe.IMAGE_FILE_MACHINE_ARM64 {
		return nil, errors.New("unsupported Windows router executable architecture")
	}
	header, ok := image.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || (header.Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_CUI && header.Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI) {
		return nil, errors.New("unsupported Windows router executable subsystem")
	}
	if len(raw) < 64 || string(raw[:2]) != "MZ" {
		return nil, errors.New("router executable has no DOS header")
	}
	peOffset := uint64(binary.LittleEndian.Uint32(raw[0x3c:0x40]))
	// Subsystem is 68 bytes into the PE32+ optional header, following the
	// four-byte PE signature and 20-byte COFF header.
	subsystemOffset := peOffset + 4 + 20 + 68
	if subsystemOffset+2 > uint64(len(raw)) || peOffset+4 > uint64(len(raw)) ||
		string(raw[peOffset:peOffset+4]) != "PE\x00\x00" {
		return nil, errors.New("router executable has an invalid PE header offset")
	}
	result := bytes.Clone(raw)
	binary.LittleEndian.PutUint16(result[subsystemOffset:subsystemOffset+2], subsystem)
	return result, nil
}

func configureBackgroundLogging(dataDir string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	image, err := pe.Open(executable)
	if err != nil {
		return err
	}
	header, ok := image.OptionalHeader.(*pe.OptionalHeader64)
	image.Close()
	if !ok || header.Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
		return nil
	}
	if err := ensureDir(dataDir); err != nil {
		return err
	}
	logPath := filepath.Join(dataDir, "service.log")
	if info, err := os.Stat(logPath); err == nil && info.Size() > 1<<20 {
		_ = os.Rename(logPath, filepath.Join(dataDir, "service.previous.log"))
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	os.Stdout, os.Stderr = log, log
	return nil
}

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
		return installWindowsService(binaryPath, dataDir, windowsTaskMatches, run)
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

func installWindowsService(binaryPath, dataDir string,
	query func(string, string) (bool, bool, error), execute func(string, ...string) error) error {
	exists, matches, err := query(binaryPath, dataDir)
	if err != nil {
		return err
	}
	if exists {
		if !matches {
			return errors.New("existing Codex Model Router task belongs to another installation")
		}
		return execute("schtasks.exe", "/Run", "/TN", "Codex Model Router")
	}
	taskXML, err := windowsTaskXML(binaryPath, dataDir)
	if err != nil {
		return err
	}
	taskPath := filepath.Join(dataDir, "windows-task.xml")
	if err := atomicWrite(taskPath, encodeUTF16LE(taskXML), 0o600); err != nil {
		return err
	}
	if err := execute("schtasks.exe", "/Create", "/F", "/TN", "Codex Model Router", "/XML", taskPath); err != nil {
		return err
	}
	return execute("schtasks.exe", "/Run", "/TN", "Codex Model Router")
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
