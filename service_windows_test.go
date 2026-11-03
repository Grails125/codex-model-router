package main

import (
	"bytes"
	"context"
	"debug/pe"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWindowsBackgroundExecutableRunsWithoutConsole(t *testing.T) {
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	installed := filepath.Join(root, "codex-router.exe")
	if err := installBackgroundBinary(source, installed, false); err != nil {
		t.Fatal(err)
	}
	background := installed
	image, err := pe.Open(background)
	if err != nil {
		t.Fatal(err)
	}
	if image.OptionalHeader.(*pe.OptionalHeader64).Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
		t.Fatal("installed background companion has a console subsystem")
	}
	image.Close()
	console, err := pe.Open(windowsConsoleBinaryPath(installed))
	if err != nil {
		t.Fatal(err)
	}
	if console.OptionalHeader.(*pe.OptionalHeader64).Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_CUI {
		t.Fatal("interactive companion does not have a console subsystem")
	}
	console.Close()
	resultPath := filepath.Join(root, "console-window.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, background, "-test.run=^TestWindowsBackgroundConsoleProbe$")
	child.Env = append(os.Environ(), "CODEX_ROUTER_CONSOLE_PROBE="+resultPath)
	child.WaitDelay = 10 * time.Second
	if err := child.Run(); err != nil {
		t.Fatalf("background companion did not execute: %v", err)
	}
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "no-console" {
		t.Fatalf("background companion allocated a console: %s", result)
	}
}

func TestWindowsInstalledConsoleCanReinstallItself(t *testing.T) {
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(t.TempDir(), "codex-router.exe")
	if err := installBackgroundBinary(source, installed, false); err != nil {
		t.Fatal(err)
	}
	console := windowsConsoleBinaryPath(installed)
	raw, err := os.ReadFile(console)
	if err != nil {
		t.Fatal(err)
	}
	// Match commandInstall's console-copy step before configuring the GUI image.
	if err := os.WriteFile(installed, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, console, "-test.run=^TestWindowsBackgroundReinstallProbe$")
	child.Env = append(os.Environ(), "CODEX_ROUTER_REINSTALL_PROBE="+installed)
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW for this console test child.
	if err := child.Run(); err != nil {
		t.Fatalf("running console companion could not reinstall itself: %v", err)
	}
	image, err := pe.Open(installed)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	if image.OptionalHeader.(*pe.OptionalHeader64).Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
		t.Fatal("console self-reinstall did not restore the GUI service image")
	}
}

func TestWindowsBackgroundReinstallProbe(t *testing.T) {
	installed := os.Getenv("CODEX_ROUTER_REINSTALL_PROBE")
	if installed == "" {
		return
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := installBackgroundBinary(source, installed, false); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsRollbackWaitsForLockedBackgroundExecutable(t *testing.T) {
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	installed := filepath.Join(root, "codex-router.exe")
	oldImage, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, oldImage, 0o700); err != nil {
		t.Fatal(err)
	}
	background := installed
	console := windowsConsoleBinaryPath(installed)
	snapshots, err := snapshotFiles(installed, console)
	if err != nil {
		t.Fatal(err)
	}
	if err := installBackgroundBinary(source, installed, false); err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(root, "locked-child-ready")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, background, "-test.run=^TestWindowsBackgroundLockProbe$")
	child.Env = append(os.Environ(), "CODEX_ROUTER_LOCK_PROBE="+readyPath)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background child did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Remove(background); err == nil {
		t.Fatal("running Windows companion unexpectedly allowed deletion")
	}
	stop := time.AfterFunc(150*time.Millisecond, func() { _ = child.Process.Kill() })
	defer stop.Stop()
	if err := restoreSnapshots(snapshots); err != nil {
		t.Fatalf("rollback did not wait for Windows to release the new companion: %v", err)
	}
	if _, err := os.Stat(console); !os.IsNotExist(err) {
		t.Fatal("rollback retained the new console companion")
	}
	if restored, err := os.ReadFile(installed); err != nil || !bytes.Equal(restored, oldImage) {
		t.Fatal("rollback did not restore the exact previous task executable")
	}
}

func TestWindowsBackgroundLockProbe(t *testing.T) {
	readyPath := os.Getenv("CODEX_ROUTER_LOCK_PROBE")
	if readyPath == "" {
		return
	}
	if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
		os.Exit(2)
	}
	time.Sleep(30 * time.Second)
}

func TestWindowsBackgroundConsoleProbe(t *testing.T) {
	resultPath := os.Getenv("CODEX_ROUTER_CONSOLE_PROBE")
	if resultPath == "" {
		return
	}
	consoleWindow, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	result := "no-console"
	if consoleWindow != 0 {
		result = "has-console"
	}
	if err := os.WriteFile(resultPath, []byte(result), 0o600); err != nil {
		os.Exit(2)
	}
}
