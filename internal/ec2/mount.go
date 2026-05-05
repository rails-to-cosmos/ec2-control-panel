package ec2

import (
	"context"
	"fmt"
	"time"

	"ec2cp/internal/config"
	"ec2cp/internal/progress"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
)

const efsCreationPollInterval = 5 * time.Second

// Mount creates an EFS mount target for the running instance. confirmer is
// called when the named EFS volume doesn't exist; passing nil suppresses
// auto-creation entirely.
func Mount(
	ctx context.Context,
	env *config.EnvConfig,
	sessionID, volumeName, az string,
	yes bool,
	confirmer func(prompt, sessionID string, yes bool) error,
) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	ec2Client := awsec2.NewFromConfig(awsCfg)
	efsClient := efs.NewFromConfig(awsCfg)

	fsID, err := findEFSByCreationToken(ctx, efsClient, volumeName)
	if err != nil {
		return fmt.Errorf("describe-file-systems: %w", err)
	}
	if fsID == "" {
		if confirmer == nil {
			return fmt.Errorf("EFS volume %q not found and auto-create is disabled", volumeName)
		}
		prompt := fmt.Sprintf("EFS volume %q not found in %s. Create one? [y/N]: ", volumeName, env.Region)
		if err := confirmer(prompt, sessionID, yes); err != nil {
			return err
		}
		fsID, err = createEFS(ctx, efsClient, volumeName)
		if err != nil {
			return err
		}
	}
	progress.Logf(ctx, "EFS file system: %s\n", fsID)

	_, instanceID, err := GetVolume(ctx, ec2Client, sessionID, az)
	if err != nil {
		return fmt.Errorf("volume lookup: %w", err)
	}
	if instanceID == "" {
		return fmt.Errorf("no running instance for %q", sessionID)
	}
	subnetID, err := GetSubnetID(ctx, ec2Client, env.VPCID, az)
	if err != nil {
		return fmt.Errorf("subnet lookup: %w", err)
	}
	if subnetID == "" {
		return fmt.Errorf("no subnet found for VPC %s in AZ %s", env.VPCID, az)
	}

	mt, err := efsClient.CreateMountTarget(ctx, &efs.CreateMountTargetInput{
		FileSystemId:   aws.String(fsID),
		SubnetId:       aws.String(subnetID),
		SecurityGroups: []string{env.SecurityGroup},
	})
	if err != nil {
		return fmt.Errorf("create-mount-target: %w", err)
	}
	progress.Logf(ctx, "Mount target %s created for instance %s on subnet %s\n",
		aws.ToString(mt.MountTargetId), instanceID, subnetID)
	return nil
}

func findEFSByCreationToken(ctx context.Context, c *efs.Client, token string) (string, error) {
	out, err := c.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{
		CreationToken: aws.String(token),
	})
	if err != nil {
		return "", err
	}
	if len(out.FileSystems) == 0 {
		return "", nil
	}
	return aws.ToString(out.FileSystems[0].FileSystemId), nil
}

// createEFS creates an encrypted EFS file system tagged with name, polls
// until LifeCycleState=available, then applies a 30-day TransitionToIA policy.
func createEFS(ctx context.Context, c *efs.Client, name string) (string, error) {
	out, err := c.CreateFileSystem(ctx, &efs.CreateFileSystemInput{
		Encrypted:     aws.Bool(true),
		CreationToken: aws.String(name),
		Tags: []efstypes.Tag{
			{Key: aws.String("Name"), Value: aws.String(name)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create-file-system: %w", err)
	}
	fsID := aws.ToString(out.FileSystemId)
	progress.Logf(ctx, "Created file system %s — waiting for available state\n", fsID)

	for {
		desc, err := c.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{
			FileSystemId: aws.String(fsID),
		})
		if err != nil {
			return "", err
		}
		if len(desc.FileSystems) == 0 {
			return "", fmt.Errorf("file system %s vanished", fsID)
		}
		state := desc.FileSystems[0].LifeCycleState
		switch state {
		case efstypes.LifeCycleStateAvailable:
			goto ready
		case efstypes.LifeCycleStateCreating:
			// keep polling
		default:
			return "", fmt.Errorf("file system %s in unexpected state %s", fsID, state)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(efsCreationPollInterval):
		}
	}
ready:
	if _, err := c.PutLifecycleConfiguration(ctx, &efs.PutLifecycleConfigurationInput{
		FileSystemId: aws.String(fsID),
		LifecyclePolicies: []efstypes.LifecyclePolicy{
			{TransitionToIA: efstypes.TransitionToIARulesAfter30Days},
		},
	}); err != nil {
		return "", fmt.Errorf("put-lifecycle-configuration: %w", err)
	}
	return fsID, nil
}
