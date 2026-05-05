package cli

import (
	"ec2cp/internal/config"
	"ec2cp/internal/ec2"

	"github.com/spf13/cobra"
)

func mountCmd() *cobra.Command {
	var (
		yes              bool
		availabilityZone string
	)
	cmd := &cobra.Command{
		Use:   "mount <volume-name> <session-id>",
		Short: "Create an EFS mount target for the running instance",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeName, sessionID := args[0], args[1]
			env, err := config.LoadEnv()
			if err != nil {
				return err
			}
			inst, err := config.GetInstance(sessionID)
			if err != nil {
				return err
			}
			az := ec2.FirstNonEmpty(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone)
			return ec2.Mount(cmd.Context(), env, sessionID, inst.AWSName(sessionID), volumeName, az, yes, ConfirmPrompt)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt for EFS creation")
	cmd.Flags().StringVarP(&availabilityZone, "availability-zone", "a", "", "AZ override (defaults to instance config or EC2_AVAILABILITY_ZONE)")
	return cmd
}
