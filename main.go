package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	loadDotenv()

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

	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func statusCmd() *cobra.Command {
	var availabilityZone string
	cmd := &cobra.Command{
		Use:   "status <session-id>",
		Short: "Show current state for the EC2 instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			env, err := loadEnvConfig()
			if err != nil {
				return err
			}
			inst, err := getInstanceConfig(sessionID)
			if err != nil {
				return err
			}
			az := firstNonEmpty(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone)
			return runStatus(cmd.Context(), env, sessionID, az)
		},
	}
	cmd.Flags().StringVarP(&availabilityZone, "availability-zone", "a", "", "AZ override (defaults to instance config or EC2_AVAILABILITY_ZONE)")
	return cmd
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
