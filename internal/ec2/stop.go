package ec2

import (
	"context"
	"fmt"
	"time"

	"ec2cp/internal/config"
	"ec2cp/internal/progress"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
)

const waitMaxDuration = 10 * time.Minute

// Stop terminates the running instance for sessionID. AWS lookups use awsName
// (= sessionID unless instances.json overrides it). When force is true and
// no volume attachment is found, falls back to a Name-tag instance lookup
// (orphan recovery). The yes flag bypasses the interactive confirmation.
func Stop(ctx context.Context, env *config.EnvConfig, sessionID, awsName, az string, force, yes bool, confirmer func(prompt, sessionID string, yes bool) error) error {
	client, err := NewClient(ctx, env.Region)
	if err != nil {
		return err
	}

	volumeID, attachedInstanceID, err := GetVolume(ctx, client, awsName, az)
	if err != nil {
		return fmt.Errorf("volume lookup: %w", err)
	}
	if volumeID != "" {
		progress.Logf(ctx, "Volume %q found: %s\n", awsName, volumeID)
	}

	eniID, err := GetENIID(ctx, client, awsName, az)
	if err != nil {
		return fmt.Errorf("eni lookup: %w", err)
	}

	var instanceIDs []string
	if attachedInstanceID != "" {
		instanceIDs = []string{attachedInstanceID}
	} else if force {
		progress.Logf(ctx, "No volume attachment — falling back to Name-tag lookup\n")
		found, err := findInstancesByName(ctx, client, awsName, az)
		if err != nil {
			return fmt.Errorf("name-tag lookup: %w", err)
		}
		instanceIDs = found
		if len(instanceIDs) > 0 {
			progress.Logf(ctx, "Found %d instance(s) in %s with Name=%q:\n", len(instanceIDs), az, awsName)
			for _, id := range instanceIDs {
				progress.Logf(ctx, "  - %s\n", id)
			}
			if confirmer != nil {
				if err := confirmer("Terminate the above? [y/N]: ", sessionID, yes); err != nil {
					return err
				}
			}
		}
	}

	if len(instanceIDs) == 0 {
		switch {
		case volumeID == "" && eniID == "":
			progress.Logf(ctx, "Nothing to stop for %q.\n", sessionID)
		case attachedInstanceID == "" && !force:
			progress.Logf(ctx, "No instance attached to volume %s — nothing to terminate. Pass --force to look up by Name tag.\n", volumeID)
		default:
			progress.Logf(ctx, "No instance found for %q; volume/ENI already detached.\n", sessionID)
		}
		return nil
	}

	for _, id := range instanceIDs {
		spotReqID, err := getSpotRequestID(ctx, client, id)
		if err != nil {
			return fmt.Errorf("spot tag lookup for %s: %w", id, err)
		}
		if spotReqID != "" {
			progress.Logf(ctx, "Cancelling spot request %s (instance %s)\n", spotReqID, id)
			if _, err := client.CancelSpotInstanceRequests(ctx, &awsec2.CancelSpotInstanceRequestsInput{
				SpotInstanceRequestIds: []string{spotReqID},
			}); err != nil {
				return fmt.Errorf("cancel spot request %s: %w", spotReqID, err)
			}
		}
		progress.Logf(ctx, "Terminating instance %s\n", id)
		if _, err := client.TerminateInstances(ctx, &awsec2.TerminateInstancesInput{
			InstanceIds: []string{id},
		}); err != nil {
			return fmt.Errorf("terminate %s: %w", id, err)
		}
	}

	progress.Logf(ctx, "Waiting for %d instance(s) to terminate\n", len(instanceIDs))
	termWaiter := awsec2.NewInstanceTerminatedWaiter(client)
	if err := termWaiter.Wait(ctx, &awsec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	}, waitMaxDuration); err != nil {
		return fmt.Errorf("wait instance-terminated: %w", err)
	}

	if volumeID != "" {
		progress.Logf(ctx, "Waiting for volume %s to become available\n", volumeID)
		volWaiter := awsec2.NewVolumeAvailableWaiter(client)
		if err := volWaiter.Wait(ctx, &awsec2.DescribeVolumesInput{
			VolumeIds: []string{volumeID},
		}, waitMaxDuration); err != nil {
			return fmt.Errorf("wait volume-available: %w", err)
		}
	}
	if eniID != "" {
		progress.Logf(ctx, "Waiting for ENI %s to become available\n", eniID)
		eniWaiter := awsec2.NewNetworkInterfaceAvailableWaiter(client)
		if err := eniWaiter.Wait(ctx, &awsec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: []string{eniID},
		}, waitMaxDuration); err != nil {
			return fmt.Errorf("wait network-interface-available: %w", err)
		}
	}

	progress.Logf(ctx, "Stopped %q.\n", sessionID)
	return nil
}
