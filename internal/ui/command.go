package ui

import "os/exec"

func runCommand(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}
