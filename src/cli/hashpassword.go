package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"ec2cp/src/server"

	"github.com/spf13/cobra"
)

// hashPasswordCmd mints a PBKDF2 hash for an EC2CP_USERS entry.
func hashPasswordCmd() *cobra.Command {
	var username string
	cmd := &cobra.Command{
		Use:   "hash-password",
		Short: "Hash a password for the EC2CP_USERS env var",
		Long: "Read a password from stdin and print its pbkdf2_sha256 hash. " +
			"With --username, print a ready \"user:hash\" entry for EC2CP_USERS. " +
			"Note: stdin input is echoed; pipe it in or clear your terminal after.",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(os.Stderr, "Password: ")
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && line == "" {
				return err
			}
			password := strings.TrimRight(line, "\r\n")
			if password == "" {
				return fmt.Errorf("empty password")
			}
			encoded := server.HashPassword(password)
			if username != "" {
				fmt.Printf("%s:%s\n", username, encoded)
			} else {
				fmt.Println(encoded)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "prepend \"username:\" to the output")
	return cmd
}
