package main

import (
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
)

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
