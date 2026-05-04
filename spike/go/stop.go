package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
)

const waitMaxDuration = 10 * time.Minute

func stopCmd() *cobra.Command {
	var (
		yes    bool
		availabilityZone string
	)
	cmd := &cobra.Command{
		Use:   "stop <session-id>",
		Short: "Stop running instance",
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
			if err := confirmDestructive(sessionID, "stop", yes); err != nil {
				return err
			}
			az := firstNonEmpty(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone)
			return runStop(cmd.Context(), env, sessionID, az)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().StringVarP(&availabilityZone, "availability-zone", "a", "", "AZ override (defaults to instance config or EC2_AVAILABILITY_ZONE)")
	return cmd
}

func runStop(ctx context.Context, env *EnvConfig, sessionID, az string) error {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(env.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	client := ec2.NewFromConfig(awsCfg)

	volumeID, instanceID, err := getVolume(ctx, client, sessionID, az)
	if err != nil {
		return fmt.Errorf("volume lookup: %w", err)
	}
	if volumeID == "" {
		fmt.Printf("Volume %q not found — nothing to stop.\n", sessionID)
		return nil
	}
	fmt.Printf("Volume %q found: %s\n", sessionID, volumeID)

	eniID, err := getENIID(ctx, client, sessionID, az)
	if err != nil {
		return fmt.Errorf("eni lookup: %w", err)
	}

	if instanceID == "" {
		fmt.Printf("No instance attached to volume %s — nothing to terminate.\n", volumeID)
		return nil
	}
	fmt.Printf("Instance found: %s\n", instanceID)

	spotRequestID, err := getSpotRequestID(ctx, client, instanceID)
	if err != nil {
		return fmt.Errorf("spot tag lookup: %w", err)
	}

	if spotRequestID != "" {
		fmt.Printf("Cancelling spot request %s\n", spotRequestID)
		if _, err := client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{spotRequestID},
		}); err != nil {
			return fmt.Errorf("cancel spot request: %w", err)
		}
	}

	fmt.Printf("Terminating instance %s\n", instanceID)
	if _, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		return fmt.Errorf("terminate: %w", err)
	}

	fmt.Printf("Waiting for volume %s to become available\n", volumeID)
	volWaiter := ec2.NewVolumeAvailableWaiter(client)
	if err := volWaiter.Wait(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{volumeID},
	}, waitMaxDuration); err != nil {
		return fmt.Errorf("wait volume-available: %w", err)
	}

	if eniID != "" {
		fmt.Printf("Waiting for ENI %s to become available\n", eniID)
		eniWaiter := ec2.NewNetworkInterfaceAvailableWaiter(client)
		if err := eniWaiter.Wait(ctx, &ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: []string{eniID},
		}, waitMaxDuration); err != nil {
			return fmt.Errorf("wait network-interface-available: %w", err)
		}
	}

	fmt.Printf("Stopped %q.\n", sessionID)
	return nil
}

func getSpotRequestID(ctx context.Context, c *ec2.Client, instanceID string) (string, error) {
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return "", err
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return "", nil
	}
	inst := out.Reservations[0].Instances[0]
	if inst.InstanceLifecycle != types.InstanceLifecycleTypeSpot {
		return "", nil
	}
	for _, t := range inst.Tags {
		if aws.ToString(t.Key) == "spot-request-id" {
			return aws.ToString(t.Value), nil
		}
	}
	return "", fmt.Errorf("instance %s is spot but has no spot-request-id tag", instanceID)
}
