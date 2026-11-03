package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	Version      = "1.0.3"
	defaultPort  = 49627
	serviceLabel = "io.openai.codex-model-router"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "install":
		err = commandInstall(os.Args[2:])
	case "uninstall":
		err = commandUninstall(os.Args[2:])
	case "serve":
		err = commandServe(os.Args[2:])
	case "status":
		err = commandStatus(os.Args[2:])
	case "doctor":
		err = commandDoctor(os.Args[2:])
	case "set-key":
		err = commandSetKey(os.Args[2:])
	case "version":
		fmt.Printf("Codex Model Router %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
	case "help", "--help", "-h":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`Codex Model Router

Commands:
  install    Configure Codex and install the hidden logon service
  uninstall  Stop the service and restore the previous provider settings
  serve      Run the loopback router in the foreground
  status     Check the router and report safe configuration state
  doctor     Validate runtime, credentials, config and service health
  set-key    Replace the DeepSeek key from --stdin or DEEPSEEK_API_KEY
  version    Print the binary version and platform

Install options:
  --port 49627
  --codex-home PATH
  --data-dir PATH
  --no-service
  --dry-run

Set DEEPSEEK_API_KEY in the environment for first-time installation. The
installer writes it to a user-only key file, not to config.toml or its output.
Use the change-deepseek-key scripts for hidden input when changing a key.
`)
}

func defaultCodexHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

func ensureDir(path string) error {
	return os.MkdirAll(path, 0o700)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func newRouterKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func readSettings(dataDir string) (RuntimeOptions, error) {
	var options RuntimeOptions
	raw, err := os.ReadFile(filepath.Join(dataDir, "settings.json"))
	if err != nil {
		return options, err
	}
	if err := json.Unmarshal(raw, &options); err != nil {
		return options, errors.New("invalid settings.json")
	}
	return normalizedRuntime(options), nil
}

func readReceipt(dataDir string) (*InstallReceipt, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "install.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt InstallReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return nil, errors.New("invalid install receipt")
	}
	return &receipt, nil
}

func extractDeepSeekKey(config string) string {
	section := ""
	for _, line := range strings.Split(strings.ReplaceAll(config, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			section = trimmed
			continue
		}
		if section != "[model_providers.deepseek]" || !strings.Contains(trimmed, "=") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "experimental_bearer_token" {
			if parsed, err := strconvUnquote(value); err == nil {
				return parsed
			}
		}
		if key == "env_key" {
			if parsed, err := strconvUnquote(value); err == nil {
				return os.Getenv(parsed)
			}
		}
	}
	return ""
}

func strconvUnquote(value string) (string, error) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", errors.New("not a quoted string")
	}
	var parsed string
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return "", err
	}
	return parsed, nil
}

func resolveDeepSeekKey(dataDir, config string) (string, error) {
	if value := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")); value != "" {
		return value, nil
	}
	if raw, err := os.ReadFile(filepath.Join(dataDir, "deepseek.key")); err == nil {
		if value := strings.TrimSpace(string(raw)); value != "" {
			return value, nil
		}
	}
	if value := extractDeepSeekKey(config); value != "" {
		return value, nil
	}
	return "", errors.New("DeepSeek credential missing; set DEEPSEEK_API_KEY for the first install")
}

func commandInstall(args []string) (installErr error) {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	port := flags.Int("port", defaultPort, "")
	codexHome := flags.String("codex-home", defaultCodexHome(), "")
	dataDir := flags.String("data-dir", "", "")
	noService := flags.Bool("no-service", false, "")
	dryRun := flags.Bool("dry-run", false, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(*codexHome, "codex-model-router")
	}
	if *port < 1024 || *port > 65535 {
		return errors.New("port must be between 1024 and 65535")
	}
	configPath := filepath.Join(*codexHome, "config.toml")
	original, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read config.toml: %w", err)
	}
	deepSeekKey, err := resolveDeepSeekKey(*dataDir, string(original))
	if err != nil {
		return err
	}
	previous, err := readReceipt(*dataDir)
	if err != nil {
		return err
	}
	existing, existingErr := readSettings(*dataDir)
	routerKey := existing.RouterKey
	if existingErr != nil || len(routerKey) < 24 {
		routerKey, err = newRouterKey()
		if err != nil {
			return fmt.Errorf("generate router key: %w", err)
		}
	}
	selectedPort, err := selectInstallPort(*port, existing, existingErr == nil)
	if err != nil {
		return err
	}
	*port = selectedPort
	activated, receipt, err := ActivateConfig(original, InstallOptions{
		Port: *port, RouterKey: routerKey, Previous: previous, InstalledAt: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("activate config: %w", err)
	}
	if *dryRun {
		fmt.Println("Dry run passed: configuration generation and validation succeeded.")
		return nil
	}
	if err := ensureDir(*dataDir); err != nil {
		return err
	}
	binaryName := "codex-router"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	installedBinary := filepath.Join(*dataDir, binaryName)
	// Snapshot before any writes so a failed upgrade cannot rotate the key or
	// receipt beneath a still-running installation.
	snapshotPaths := []string{configPath, filepath.Join(*dataDir, "settings.json"),
		filepath.Join(*dataDir, "install.json"), filepath.Join(*dataDir, "deepseek.key"), installedBinary}
	if runtime.GOOS == "windows" {
		snapshotPaths = append(snapshotPaths, windowsConsoleBinaryPath(installedBinary), filepath.Join(*dataDir, "windows-task.xml"))
	}
	rollbackFiles, err := snapshotFiles(snapshotPaths...)
	if err != nil {
		return err
	}
	serviceStopped := false
	defer func() {
		if installErr == nil {
			return
		}
		if !*noService && serviceStopped {
			_ = stopInstalledService(installedBinary, *dataDir)
		}
		if restoreErr := restoreSnapshots(rollbackFiles); restoreErr != nil {
			installErr = fmt.Errorf("%w; rollback failed: %v", installErr, restoreErr)
			return
		}
		if serviceStopped && previous != nil && existingErr == nil {
			if restartErr := installService(installedBinary, *dataDir); restartErr != nil {
				installErr = fmt.Errorf("%w; restart previous service failed: %v", installErr, restartErr)
			}
		}
	}()
	backupDir := filepath.Join(*dataDir, "backups")
	if err := ensureDir(backupDir); err != nil {
		return err
	}
	backupPath := filepath.Join(backupDir, time.Now().Format("20060102-150405")+"-config.toml")
	if err := atomicWrite(backupPath, original, 0o600); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(*dataDir, "deepseek.key"), []byte(deepSeekKey), 0o600); err != nil {
		return err
	}
	options := normalizedRuntime(RuntimeOptions{
		Port: *port, RouterKey: routerKey, Version: Version,
	})
	settingsRaw, _ := json.MarshalIndent(options, "", "  ")
	if err := atomicWrite(filepath.Join(*dataDir, "settings.json"), settingsRaw, 0o600); err != nil {
		return err
	}
	receipt.Version = Version
	receipt.OS = runtime.GOOS
	receipt.Arch = runtime.GOARCH
	receipt.ConfigPath = configPath
	receipt.BackupPath = backupPath
	receipt.InstalledAt = time.Now().UTC()
	receiptRaw, _ := json.MarshalIndent(receipt, "", "  ")
	if err := atomicWrite(filepath.Join(*dataDir, "install.json"), receiptRaw, 0o600); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	sourceAbsolute, _ := filepath.Abs(executable)
	targetAbsolute, _ := filepath.Abs(installedBinary)
	if !*noService {
		if err := stopInstalledService(installedBinary, *dataDir); err != nil {
			return fmt.Errorf("stop previous router: %w", err)
		}
		serviceStopped = true
	}
	if sourceAbsolute != targetAbsolute {
		raw, err := os.ReadFile(executable)
		if err != nil {
			return err
		}
		if err := replaceBinary(installedBinary, raw); err != nil {
			return err
		}
	}
	if err := installBackgroundBinary(executable, installedBinary, *noService); err != nil {
		return fmt.Errorf("install background executable: %w", err)
	}
	if err := atomicWrite(configPath, activated, 0o600); err != nil {
		return err
	}
	if !*noService {
		if err := installService(installedBinary, *dataDir); err != nil {
			return fmt.Errorf("install background service: %w", err)
		}
		if err := waitForHealth(options, 20*time.Second); err != nil {
			return fmt.Errorf("router health check: %w", err)
		}
	}
	result := map[string]any{
		"status": "installed", "version": Version, "provider": "codex-router",
		"port": *port, "data_dir": *dataDir, "config": configPath,
		"backup": backupPath, "service": !*noService,
		"next": "Restart Codex and start a new chat after finishing active work.",
	}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(encoded))
	return nil
}

func commandServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	codexHome := flags.String("codex-home", defaultCodexHome(), "")
	dataDir := flags.String("data-dir", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(*codexHome, "codex-model-router")
	}
	if err := configureBackgroundLogging(*dataDir); err != nil {
		return err
	}
	options, err := readSettings(*dataDir)
	if err != nil {
		return fmt.Errorf("read router settings: %w", err)
	}
	options.Version = Version // Report the running executable, not stale installer metadata.
	audit := newAuditLogger(filepath.Join(*dataDir, "requests.jsonl"))
	router, err := NewRouter(options, func() (string, error) {
		return readDeepSeekKey(*dataDir)
	}, audit.Write)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: fmt.Sprintf("127.0.0.1:%d", options.Port), Handler: router,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	fmt.Printf("Codex Model Router %s listening on http://127.0.0.1:%d\n", Version, options.Port)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type auditLogger struct {
	mu   sync.Mutex
	path string
}

func newAuditLogger(path string) *auditLogger {
	return &auditLogger{path: path}
}

func (logger *auditLogger) Write(record AuditRecord) {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if info, err := os.Stat(logger.path); err == nil && info.Size() > 2<<20 {
		_ = os.Rename(logger.path, strings.TrimSuffix(logger.path, ".jsonl")+".previous.jsonl")
	}
	raw, _ := json.Marshal(record)
	file, err := os.OpenFile(logger.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(raw, '\n'))
}

type Health struct {
	Service string `json:"service"`
	Version string `json:"version"`
	Models  int    `json:"models"`
	Proof   string `json:"proof,omitempty"`
}

func checkHealth(options RuntimeOptions) (Health, error) {
	var health Health
	challenge, err := newRouterKey()
	if err != nil {
		return health, err
	}
	request, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/health?challenge=%s", options.Port, challenge), nil)
	if err != nil {
		return health, err
	}
	request.Header.Set("X-Codex-Router-Key", options.RouterKey)
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return health, errors.New("router is not reachable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return health, errors.New("router authentication or health check failed")
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&health); err != nil {
		return health, errors.New("invalid router health response")
	}
	if health.Service != "codex-model-router" || health.Version != options.Version ||
		!constantTimeEqual(health.Proof, healthProof(options.RouterKey, challenge)) {
		return health, errors.New("router instance or version does not match this installation")
	}
	health.Proof = ""
	return health, nil
}

func waitForHealth(options RuntimeOptions, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := checkHealth(options); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("configured router instance did not become healthy")
}

func selectInstallPort(preferred int, existing RuntimeOptions, hasExisting bool) (int, error) {
	if hasExisting && (existing.Port == preferred || preferred == defaultPort) {
		if _, err := checkHealth(existing); err == nil {
			return existing.Port, nil
		}
	}
	for candidate := preferred; candidate <= 65535 && candidate < preferred+64; candidate++ {
		listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", candidate))
		if err == nil {
			listener.Close()
			return candidate, nil
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, errors.New("no loopback port available")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port, nil
}

type fileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func snapshotFiles(paths ...string) ([]fileSnapshot, error) {
	var snapshots []fileSnapshot
	for _, path := range paths {
		item := fileSnapshot{path: path}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			snapshots = append(snapshots, item)
			continue
		}
		if err != nil {
			return nil, err
		}
		item.data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		item.mode, item.exists = info.Mode().Perm(), true
		snapshots = append(snapshots, item)
	}
	return snapshots, nil
}

func restoreSnapshots(snapshots []fileSnapshot) error {
	var result error
	for _, item := range snapshots {
		var err error
		if item.exists {
			current, readErr := os.ReadFile(item.path)
			if readErr == nil && string(current) == string(item.data) {
				continue
			}
			err = retryAtomicWrite(item.path, item.data, item.mode)
		} else {
			err = retryRemove(item.path)
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("restore %s: %w", filepath.Base(item.path), err))
		}
	}
	return result
}

func retryRemove(path string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func replaceBinary(path string, raw []byte) error {
	return retryAtomicWrite(path, raw, 0o700)
}

func retryAtomicWrite(path string, raw []byte, mode os.FileMode) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := atomicWrite(path, raw, mode)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func commandStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	codexHome := flags.String("codex-home", defaultCodexHome(), "")
	dataDir := flags.String("data-dir", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(*codexHome, "codex-model-router")
	}
	options, err := readSettings(*dataDir)
	if err != nil {
		return err
	}
	options.Version = Version
	health, err := checkHealth(options)
	if err != nil {
		return err
	}
	result := map[string]any{"healthy": true, "health": health, "data_dir": *dataDir}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(encoded))
	return nil
}

func commandDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	codexHome := flags.String("codex-home", defaultCodexHome(), "")
	dataDir := flags.String("data-dir", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(*codexHome, "codex-model-router")
	}
	configRaw, configErr := os.ReadFile(filepath.Join(*codexHome, "config.toml"))
	options, settingsErr := readSettings(*dataDir)
	keyMissing := false
	if raw, err := os.ReadFile(filepath.Join(*dataDir, "deepseek.key")); err != nil || strings.TrimSpace(string(raw)) == "" {
		keyMissing = true
	}
	options.Version = Version
	healthErr := waitForHealth(options, 2*time.Second)
	result := map[string]any{
		"version": Version, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"config_exists": configErr == nil, "settings_valid": settingsErr == nil,
		"deepseek_key_present": !keyMissing, "health": healthErr == nil,
		"provider_configured": configErr == nil && strings.Contains(string(configRaw), `model_provider = "codex-router"`),
	}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(encoded))
	if configErr != nil || settingsErr != nil || keyMissing || healthErr != nil || result["provider_configured"] != true {
		return errors.New("one or more checks failed")
	}
	return nil
}

func commandUninstall(args []string) error {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	codexHome := flags.String("codex-home", defaultCodexHome(), "")
	dataDir := flags.String("data-dir", "", "")
	noService := flags.Bool("no-service", false, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(*codexHome, "codex-model-router")
	}
	receipt, err := readReceipt(*dataDir)
	if err != nil {
		return err
	}
	if receipt == nil {
		return errors.New("installation receipt not found")
	}
	configPath := filepath.Join(*codexHome, "config.toml")
	current, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	restored, err := RestoreConfig(current, *receipt)
	if err != nil {
		return err
	}
	if !*noService {
		_ = stopService()
	}
	if err := atomicWrite(configPath, restored, 0o600); err != nil {
		return err
	}
	fmt.Println("Model router stopped and the original provider/catalog settings were restored.")
	fmt.Println("Backups and the installed binary remain in the data directory for recovery.")
	return nil
}

func run(name string, args ...string) error {
	command := exec.CommandContext(context.Background(), name, args...)
	raw, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(raw))
		if message == "" {
			message = err.Error()
		}
		return errors.New(message)
	}
	return nil
}
