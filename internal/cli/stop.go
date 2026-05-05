package cli

import (
	"ec2cp/internal/config"
	"ec2cp/internal/ec2"

	"github.com/spf13/cobra"
)

func stopCmd() *cobra.Command {
	var (
		yes              bool
		force            bool
		availabilityZone string
	)
	cmd := &cobra.Command{
		Use:   "stop <session-id>",
		Short: "Stop running instance",
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
			if err := ConfirmDestructive(sessionID, "stop", yes); err != nil {
				return err
			}
			az := firstNonEmpty(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone)
			return ec2.Stop(cmd.Context(), env, sessionID, az, force, yes, ConfirmPrompt)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "fall back to Name-tag lookup when no volume attachment is found (recovers orphans)")
	cmd.Flags().StringVarP(&availabilityZone, "availability-zone", "a", "", "AZ override (defaults to instance config or EC2_AVAILABILITY_ZONE)")
	return cmd
}
