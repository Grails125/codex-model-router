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
	fromStdin := flags.Bool("stdin", false, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid set-key options; use --stdin, --codex-home PATH or --data-dir PATH; never pass a key as an argument")
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(*codexHome, "codex-model-router")
	}
	if _, err := readSettings(*dataDir); err != nil {
		return errors.New("router installation not found or settings invalid; install the router first")
	}
	key := os.Getenv("DEEPSEEK_API_KEY")
	if *fromStdin {
		raw, err := io.ReadAll(io.LimitReader(input, maxKeyBytes+3))
		if err != nil {
			return errors.New("unable to read DeepSeek API key from stdin")
		}
		if len(raw) > maxKeyBytes+2 {
			return errors.New("DeepSeek API key input is too long")
		}
		key = string(raw)
	} else if key == "" {
		return errors.New("use the change-deepseek-key script for hidden input, --stdin, or the DEEPSEEK_API_KEY environment variable")
	}
	key = strings.TrimSpace(key)
	if len(key) == 0 || len(key) > maxKeyBytes {
		return errors.New("DeepSeek API key must contain 1 to 4096 printable ASCII characters without spaces")
	}
	for _, value := range []byte(key) {
		if value < 33 || value > 126 {
			return errors.New("DeepSeek API key must contain 1 to 4096 printable ASCII characters without spaces")
		}
	}
	if err := atomicWrite(filepath.Join(*dataDir, "deepseek.key"), []byte(key), 0o600); err != nil {
		return errors.New("unable to replace DeepSeek credential file; previous credential was preserved")
	}
	fmt.Fprintln(output, "DeepSeek API key saved locally. The next DeepSeek request uses it; no router or Codex restart is needed. API validity was not checked.")
	return nil
}

func readDeepSeekKey(dataDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "deepseek.key"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
