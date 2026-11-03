package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testWindowsPE(t *testing.T, machine, subsystem uint16) []byte {
	t.Helper()
	raw := make([]byte, 64)
	copy(raw, "MZ")
	binary.LittleEndian.PutUint32(raw[0x3c:], 64)
	buffer := bytes.NewBuffer(raw)
	buffer.WriteString("PE\x00\x00")
	header := pe.FileHeader{Machine: machine, SizeOfOptionalHeader: uint16(binary.Size(pe.OptionalHeader64{}))}
	if err := binary.Write(buffer, binary.LittleEndian, header); err != nil {
		t.Fatal(err)
	}
	optional := pe.OptionalHeader64{Magic: 0x20b, Subsystem: subsystem, NumberOfRvaAndSizes: 16}
	if err := binary.Write(buffer, binary.LittleEndian, optional); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestWindowsGUIBinaryChangesOnlySubsystemAndPreservesSource(t *testing.T) {
	for _, machine := range []uint16{pe.IMAGE_FILE_MACHINE_AMD64, pe.IMAGE_FILE_MACHINE_ARM64} {
		original := testWindowsPE(t, machine, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI)
		before := bytes.Clone(original)
		background, err := windowsGUIBinary(original)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, original) {
			t.Fatal("GUI conversion modified the console executable")
		}
		image, err := pe.NewFile(bytes.NewReader(background))
		if err != nil {
			t.Fatal(err)
		}
		if image.Machine != machine || image.OptionalHeader.(*pe.OptionalHeader64).Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
			t.Fatal("generated background executable is not a GUI PE of the original architecture")
		}
		image.Close()
		background[64+4+20+68] = byte(pe.IMAGE_SUBSYSTEM_WINDOWS_CUI)
		if !bytes.Equal(before, background) {
			t.Fatal("GUI conversion changed bytes outside the PE subsystem")
		}
	}
}

func TestWindowsGUIBinaryRejectsUnexpectedExecutables(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("not a PE"),
		testWindowsPE(t, pe.IMAGE_FILE_MACHINE_I386, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI),
		testWindowsPE(t, pe.IMAGE_FILE_MACHINE_AMD64, pe.IMAGE_SUBSYSTEM_NATIVE),
	} {
		if _, err := windowsGUIBinary(raw); err == nil {
			t.Fatal("accepted an unsupported or malformed executable")
		}
	}
}

func TestWindowsTaskIdentityKeepsOriginalExecutablePath(t *testing.T) {
	t.Setenv("USERDOMAIN", "TESTDOMAIN")
	t.Setenv("USERNAME", "TestUser")
	binaryPath := filepath.Join(t.TempDir(), "codex-router.exe")
	dataDir := filepath.Dir(binaryPath)
	for _, command := range []string{binaryPath, strings.ToUpper(binaryPath)} {
		raw, err := windowsTaskXML(command, dataDir)
		if err != nil {
			t.Fatal(err)
		}
		match, err := taskXMLMatches(encodeUTF16LE(raw), binaryPath, dataDir)
		if err != nil || !match {
			t.Fatalf("owned task rejected: %v", err)
		}
		if match, _ := taskXMLMatches(raw, binaryPath, dataDir+"-other"); match {
			t.Fatal("accepted task for another data directory")
		}
	}
	other, _ := windowsTaskXML(windowsConsoleBinaryPath(binaryPath), dataDir)
	if match, _ := taskXMLMatches(other, binaryPath, dataDir); match {
		t.Fatal("accepted a task pointing at the interactive console companion")
	}
}

func TestWindowsExistingTaskIsReusedWithoutReRegistration(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "codex-router.exe")
	dataDir := filepath.Dir(binaryPath)
	taskPath := filepath.Join(dataDir, "windows-task.xml")
	if err := os.WriteFile(taskPath, []byte("previous-task-registration"), 0o600); err != nil {
		t.Fatal(err)
	}
	queries, runs := 0, 0
	err := installWindowsService(binaryPath, dataDir, func(binary, data string) (bool, bool, error) {
		queries++
		if binary != binaryPath || data != dataDir {
			t.Fatal("queried a different installation")
		}
		return true, true, nil
	}, func(name string, args ...string) error {
		if name != "schtasks.exe" || strings.Join(args, " ") != "/Run /TN Codex Model Router" {
			return errors.New("access denied: existing task cannot be re-registered")
		}
		runs++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if queries != 1 || runs != 1 || string(mustRead(t, taskPath)) != "previous-task-registration" {
		t.Fatal("reinstallation did not reuse the existing scheduled task unchanged")
	}
}

func TestWindowsTaskXMLSeparatesLongCommandAndArguments(t *testing.T) {
	t.Setenv("USERDOMAIN", "TESTDOMAIN")
	t.Setenv("USERNAME", "TestUser")
	binary := `C:\` + strings.Repeat(`very-long-directory\`, 20) + `codex-router.exe`
	dataDir := `C:\` + strings.Repeat(`very-long-data-directory\`, 20)
	raw, err := windowsTaskXML(binary, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"<Command>" + binary + "</Command>",
		"<Arguments>serve --data-dir &quot;" + dataDir + "&quot;</Arguments>",
		"<LogonTrigger>",
		"<RunLevel>LeastPrivilege</RunLevel>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("task XML missing %q:\n%s", want, text)
		}
	}
	encoded := encodeUTF16LE(raw)
	if len(encoded) < 2 || encoded[0] != 0xff || encoded[1] != 0xfe {
		t.Fatal("scheduled task XML is missing a UTF-16LE BOM")
	}
}

func TestMacLaunchAgentPlistIncludesLoginStartAndLoopbackBinary(t *testing.T) {
	raw := macLaunchAgentPlist(
		"/Users/test/Library/Application Support/Codex/codex-router",
		"/Users/test/.codex/codex-model-router",
	)
	decoder := xml.NewDecoder(strings.NewReader(string(raw)))
	for {
		if _, err := decoder.Token(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("invalid launch agent plist: %v", err)
		}
	}
	text := string(raw)
	for _, want := range []string{
		"<string>" + serviceLabel + "</string>",
		"<string>--data-dir</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><true/>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("launch agent plist missing %q:\n%s", want, text)
		}
	}
}

func TestMacBootoutUsesSingleServiceTarget(t *testing.T) {
	args := macBootoutArguments("501\n")
	if len(args) != 2 || args[0] != "bootout" || args[1] != "gui/501/io.openai.codex-model-router" {
		t.Fatalf("launchctl bootout requires a single service-target, got %q", args)
	}
}
