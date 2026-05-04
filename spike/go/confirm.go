package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const testSessionID = "test"

func confirmDestructive(sessionID, action string, yes bool) error {
	return confirmPrompt(fmt.Sprintf("About to %s %q. Continue? [y/N]: ", action, sessionID), sessionID, yes)
}

// confirmPrompt prints a custom prompt and reads a y/N answer.
// Bypassed when yes is true or sessionID is the test exception.
func confirmPrompt(prompt, sessionID string, yes bool) error {
	if yes || sessionID == testSessionID {
		return nil
	}
	fmt.Fprint(os.Stderr, prompt)
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
