package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func restartCmd() *cobra.Command {
	var (
		yes          bool
		instanceType string
		requestType  string
		instanceName string
		availabilityZone       string
	)
	cmd := &cobra.Command{
		Use:   "restart <session-id>",
		Short: "Restart existing instance, optionally changing instance type",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			env, err := loadEnvConfig()
			if err != nil {
				return err
			}
			if err := env.requireForLaunch(); err != nil {
				return err
			}
			inst, err := getInstanceConfig(sessionID)
			if err != nil {
				return err
			}
			if err := confirmDestructive(sessionID, "restart", yes); err != nil {
				return err
			}

			az := firstNonEmpty(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone)
			if err := runStop(cmd.Context(), env, sessionID, az, false, true); err != nil {
				return fmt.Errorf("stop phase: %w", err)
			}

			rType, rTypeSrc := resolveSource(requestType, inst.RequestType, env.DefaultRequestType,
				"request-type", "request_type", "EC2_REQUEST_TYPE")
			iType, iTypeSrc := resolveSource(instanceType, inst.InstanceType, env.DefaultInstanceType,
				"instance-type", "instance_type", "EC2_INSTANCE_TYPE")
			_, azSrc := resolveSource(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone,
				"availability-zone", "availability_zone", "EC2_AVAILABILITY_ZONE")

			name, nameSrc := instanceName, "--instance-name"
			if name == "" {
				name, nameSrc = sessionID, "session-id default"
			}

			params := LaunchParams{
				SessionID:          sessionID,
				InstanceName:       name,
				InstanceType:       iType,
				RequestType:        rType,
				VolumeSize:         env.InstanceVolumeSize,
				Env:                env,
				AZ:                 az,
				InstanceNameSource: nameSrc,
				InstanceTypeSource: iTypeSrc,
				RequestTypeSource:  rTypeSrc,
				AZSource:           azSrc,
			}
			return runStart(cmd.Context(), params)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().StringVar(&instanceType, "instance-type", "", "instance type (overrides config + env)")
	cmd.Flags().StringVar(&requestType, "request-type", "", "spot|ondemand (overrides config + env)")
	cmd.Flags().StringVar(&instanceName, "instance-name", "", "Name tag (defaults to session-id)")
	cmd.Flags().StringVarP(&availabilityZone, "availability-zone", "a", "", "AZ override (defaults to instance config or EC2_AVAILABILITY_ZONE)")
	return cmd
}
