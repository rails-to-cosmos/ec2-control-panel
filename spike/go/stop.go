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
			return runStop(cmd.Context(), env, sessionID, az, force, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "fall back to Name-tag lookup when no volume attachment is found (recovers orphans)")
	cmd.Flags().StringVarP(&availabilityZone, "availability-zone", "a", "", "AZ override (defaults to instance config or EC2_AVAILABILITY_ZONE)")
	return cmd
}

func runStop(ctx context.Context, env *EnvConfig, sessionID, az string, force, yes bool) error {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(env.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	client := ec2.NewFromConfig(awsCfg)

	volumeID, attachedInstanceID, err := getVolume(ctx, client, sessionID, az)
	if err != nil {
		return fmt.Errorf("volume lookup: %w", err)
	}
	if volumeID != "" {
		fmt.Printf("Volume %q found: %s\n", sessionID, volumeID)
	}

	eniID, err := getENIID(ctx, client, sessionID, az)
	if err != nil {
		return fmt.Errorf("eni lookup: %w", err)
	}

	var instanceIDs []string
	if attachedInstanceID != "" {
		instanceIDs = []string{attachedInstanceID}
	} else if force {
		fmt.Println("No volume attachment — falling back to Name-tag lookup")
		found, err := findInstancesByName(ctx, client, sessionID, az)
		if err != nil {
			return fmt.Errorf("name-tag lookup: %w", err)
		}
		instanceIDs = found
		if len(instanceIDs) > 0 {
			fmt.Printf("Found %d instance(s) in %s with Name=%q:\n", len(instanceIDs), az, sessionID)
			for _, id := range instanceIDs {
				fmt.Printf("  - %s\n", id)
			}
			if err := confirmPrompt("Terminate the above? [y/N]: ", sessionID, yes); err != nil {
				return err
			}
		}
	}

	if len(instanceIDs) == 0 {
		switch {
		case volumeID == "" && eniID == "":
			fmt.Printf("Nothing to stop for %q.\n", sessionID)
		case attachedInstanceID == "" && !force:
			fmt.Printf("No instance attached to volume %s — nothing to terminate. Pass --force to look up by Name tag.\n", volumeID)
		default:
			fmt.Printf("No instance found for %q; volume/ENI already detached.\n", sessionID)
		}
		return nil
	}

	for _, id := range instanceIDs {
		spotReqID, err := getSpotRequestID(ctx, client, id)
		if err != nil {
			return fmt.Errorf("spot tag lookup for %s: %w", id, err)
		}
		if spotReqID != "" {
			fmt.Printf("Cancelling spot request %s (instance %s)\n", spotReqID, id)
			if _, err := client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
				SpotInstanceRequestIds: []string{spotReqID},
			}); err != nil {
				return fmt.Errorf("cancel spot request %s: %w", spotReqID, err)
			}
		}
		fmt.Printf("Terminating instance %s\n", id)
		if _, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: []string{id},
		}); err != nil {
			return fmt.Errorf("terminate %s: %w", id, err)
		}
	}

	fmt.Printf("Waiting for %d instance(s) to terminate\n", len(instanceIDs))
	termWaiter := ec2.NewInstanceTerminatedWaiter(client)
	if err := termWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	}, waitMaxDuration); err != nil {
		return fmt.Errorf("wait instance-terminated: %w", err)
	}

	if volumeID != "" {
		fmt.Printf("Waiting for volume %s to become available\n", volumeID)
		volWaiter := ec2.NewVolumeAvailableWaiter(client)
		if err := volWaiter.Wait(ctx, &ec2.DescribeVolumesInput{
			VolumeIds: []string{volumeID},
		}, waitMaxDuration); err != nil {
			return fmt.Errorf("wait volume-available: %w", err)
		}
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

// findInstancesByName returns the IDs of non-terminated instances with the given Name tag in the AZ.
// Used as a fallback when the persistent volume isn't attached (orphan recovery).
func findInstancesByName(ctx context.Context, c *ec2.Client, name, az string) ([]string, error) {
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:Name"), Values: []string{name}},
			{Name: aws.String("availability-zone"), Values: []string{az}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"}},
		},
	})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			ids = append(ids, aws.ToString(inst.InstanceId))
		}
	}
	return ids, nil
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
