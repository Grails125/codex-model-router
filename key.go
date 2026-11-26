package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxKeyBytes = 4096

func commandSetKey(args []string) error {
	return setKey(args, os.Stdin, os.Stdout)
}

func setKey(args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("set-key", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	codexHome := flags.String("codex-home", defaultCodexHome(), "")
	dataDir := flags.String("data-dir", "", "")
	provider := flags.String("provider", "deepseek", "")
	fromStdin := flags.Bool("stdin", false, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid set-key options; use --provider, --stdin, --codex-home or --data-dir; never pass a key as an argument")
	}
	name, filename, envKey := "DeepSeek", "deepseek.key", "DEEPSEEK_API_KEY"
	switch *provider {
	case "deepseek":
	case openCodeGoProvider:
		name, filename, envKey = "OpenCode Go", "opencode-go.key", "OPENCODE_API_KEY"
	default:
		return errors.New("set-key provider must be deepseek or opencode-go")
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(*codexHome, "codex-model-router")
	}
	if _, err := readSettings(*dataDir); err != nil {
		return errors.New("router installation not found or settings invalid; install the router first")
	}
	key := os.Getenv(envKey)
	if *fromStdin {
		raw, err := io.ReadAll(io.LimitReader(input, maxKeyBytes+3))
		if err != nil {
			return fmt.Errorf("unable to read %s API key from stdin", name)
		}
		if len(raw) > maxKeyBytes+2 {
			return fmt.Errorf("%s API key input is too long", name)
		}
		key = string(raw)
	} else if key == "" {
		if *provider == "deepseek" {
			return errors.New("use the change-deepseek-key script for hidden input, --stdin, or the DEEPSEEK_API_KEY environment variable")
		}
		return fmt.Errorf("provide the %s API key through --stdin or %s", name, envKey)
	}
	key = strings.TrimSpace(key)
	if len(key) == 0 || len(key) > maxKeyBytes {
		return fmt.Errorf("%s API key must contain 1 to 4096 printable ASCII characters without spaces", name)
	}
	for _, value := range []byte(key) {
		if value < 33 || value > 126 {
			return fmt.Errorf("%s API key must contain 1 to 4096 printable ASCII characters without spaces", name)
		}
	}
	if err := atomicWrite(filepath.Join(*dataDir, filename), []byte(key), 0o600); err != nil {
		return fmt.Errorf("unable to replace %s credential file; previous credential was preserved", name)
	}
	fmt.Fprintf(output, "%s API key saved locally. The next %s request uses it; no router or Codex restart is needed. API validity was not checked.\n", name, name)
	return nil
}

func readDeepSeekKey(dataDir string) (string, error) {
	return readProviderKey(dataDir, "deepseek.key")
}

func readProviderKey(dataDir, filename string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, filename))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
