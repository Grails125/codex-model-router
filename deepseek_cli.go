package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
)

func deepSeekCLIArgs(args []string) ([]string, error) {
	flags := flag.NewFlagSet("deepseek", flag.ContinueOnError)
	model := flags.String("model", "deepseek-flash", "DeepSeek model (deepseek-flash or deepseek-v4-pro)")
	resume := flags.String("resume", "", "Existing Codex session ID or name")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, errors.New("usage: codex-router deepseek [--model MODEL] [--resume SESSION_ID]")
	}
	registry, _, err := newModelRegistry()
	if err != nil {
		return nil, err
	}
	if !registry.isDeepSeek(*model) {
		return nil, errors.New("select deepseek-flash or deepseek-v4-pro")
	}
	if *resume != "" {
		return []string{"resume", "--model", *model, "--", *resume}, nil
	}
	return []string{"--model", *model}, nil
}

func commandDeepSeek(args []string) error {
	codexArgs, err := deepSeekCLIArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	command := exec.Command("codex", codexArgs...)
	if command.Err != nil {
		return fmt.Errorf("Codex CLI must be available on PATH: %w", command.Err)
	}
	// Use the same configuration, provider identity and session store as Codex.
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}
