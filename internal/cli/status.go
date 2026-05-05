package cli

import (
	"ec2cp/internal/config"
	"ec2cp/internal/ec2"

	"github.com/spf13/cobra"
)

func statusCmd() *cobra.Command {
	var availabilityZone string
	cmd := &cobra.Command{
		Use:   "status <session-id>",
		Short: "Show current state for the EC2 instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			env, err := config.LoadEnv()
			if err != nil {
				return err
			}
			inst, err := config.GetInstance(sessionID)
			if err != nil {
				return err
			}
			az := firstNonEmpty(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone)
			return ec2.Status(cmd.Context(), env, sessionID, az)
		},
	}
	cmd.Flags().StringVarP(&availabilityZone, "availability-zone", "a", "", "AZ override (defaults to instance config or EC2_AVAILABILITY_ZONE)")
	return cmd
}
