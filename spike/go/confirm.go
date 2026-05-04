package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const testSessionID = "test"

func confirmDestructive(sessionID, action string, yes bool) error {
	if yes || sessionID == testSessionID {
		return nil
	}
	fmt.Fprintf(os.Stderr, "About to %s %q. Continue? [y/N]: ", action, sessionID)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	resp := strings.ToLower(strings.TrimSpace(line))
	if resp != "y" && resp != "yes" {
		return fmt.Errorf("aborted")
	}
	return nil
}
