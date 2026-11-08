package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/VictorLowther/mini/config"
)

const configHelp = `mini config - manage the global config file

Usage:
  mini config <command> [args]

Commands:
  setup            re-run the first-run wizard
  set KEY VALUE    write KEY='VALUE' into the config file
  unset KEY        remove KEY from the config file
  edit             open the config file in $EDITOR (else nano)
  path             print the config file location

The config file is loaded at startup, so what it sets is available to every run
without exporting it yourself. You can also edit it by hand.`

// runConfigCommand is the `mini config` subcommand: the Go equivalent of
// mini-extra config.
func runConfigCommand(args []string, stdin *os.File, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, configHelp)
		return nil
	}
	path := config.GlobalConfigFile()
	if _, err := config.EnsureConfigDir(); err != nil {
		return err
	}
	in := bufio.NewReader(stdin)
	switch args[0] {
	case "setup":
		return setupWizard(stdin, stdout, true)
	case "set":
		key, value := "", ""
		if len(args) > 1 {
			key = args[1]
		}
		if len(args) > 2 {
			value = args[2]
		}
		if key == "" {
			var err error
			if key, err = prompt(in, stdout, "Enter the key to set", ""); err != nil {
				return err
			}
		}
		if value == "" {
			var err error
			if value, err = prompt(in, stdout, "Enter the value for "+key, ""); err != nil {
				return err
			}
		}
		if err := config.SetEnvValue(path, key, value); err != nil {
			return err
		}
		_ = os.Setenv(key, value)
		fmt.Fprintf(stdout, "Set %s in %s\n", key, path)
		return nil
	case "unset":
		key := ""
		if len(args) > 1 {
			key = args[1]
		}
		if key == "" {
			var err error
			if key, err = prompt(in, stdout, "Enter the key to unset", ""); err != nil {
				return err
			}
		}
		if err := config.DeleteEnvValue(path, key); err != nil {
			return err
		}
		_ = os.Unsetenv(key)
		fmt.Fprintf(stdout, "Unset %s in %s\n", key, path)
		return nil
	case "edit":
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "nano"
		}
		cmd := exec.Command(editor, path)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s exited with an error: %w", editor, err)
		}
		return nil
	case "path":
		fmt.Fprintln(stdout, path)
		return nil
	}
	return fmt.Errorf("unknown config command %q (try: setup, set, unset, edit, path)", args[0])
}
