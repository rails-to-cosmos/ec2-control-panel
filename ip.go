package main

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

func ipCmd() *cobra.Command {
	var availabilityZone string
	cmd := &cobra.Command{
		Use:   "ip <session-id>",
		Short: "Print the private IP of the running instance",
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

			ctx := cmd.Context()
			awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(env.Region))
			if err != nil {
				return fmt.Errorf("aws config: %w", err)
			}
			client := ec2.NewFromConfig(awsCfg)

			_, instanceID, err := getVolume(ctx, client, sessionID, az)
			if err != nil {
				return fmt.Errorf("volume lookup: %w", err)
			}
			if instanceID == "" {
				return fmt.Errorf("no running instance for %q", sessionID)
			}
			out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
				InstanceIds: []string{instanceID},
			})
			if err != nil {
				return fmt.Errorf("describe-instances: %w", err)
			}
			if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
				return fmt.Errorf("instance %s vanished", instanceID)
			}
			logf(ctx, "%s\n", aws.ToString(out.Reservations[0].Instances[0].PrivateIpAddress))
			return nil
		},
	}
	cmd.Flags().StringVarP(&availabilityZone, "availability-zone", "a", "", "AZ override (defaults to instance config or EC2_AVAILABILITY_ZONE)")
	return cmd
}
