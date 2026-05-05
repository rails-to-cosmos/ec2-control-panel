// Package cli wires the cobra subcommands. Each command is a thin wrapper
// that reads env + instance config, builds parameters, and calls into ec2.
package cli

import "github.com/spf13/cobra"

// NewRoot returns the configured root cobra command (with all subcommands attached).
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "ec2cp",
		Short: "EC2 control panel — per-user EC2 sandbox manager",
	}
	root.AddCommand(statusCmd())
	root.AddCommand(startCmd())
	root.AddCommand(stopCmd())
	root.AddCommand(restartCmd())
	root.AddCommand(ipCmd())
	root.AddCommand(mountCmd())
	root.AddCommand(serveCmd())
	return root
}
