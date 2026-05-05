package ec2

import (
	"context"
	"fmt"

	"ec2cp/internal/config"
	"ec2cp/internal/progress"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
)

const notFound = "Not found"

func Status(ctx context.Context, env *config.EnvConfig, sessionID, az string) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	client := awsec2.NewFromConfig(awsCfg)

	progress.Logf(ctx, "Session ID: %s\n", sessionID)
	progress.Logf(ctx, "VPC: %s\n", env.VPCID)
	progress.Logf(ctx, "Region: %s\n", env.Region)
	progress.Logf(ctx, "Availability zone: %s\n", az)

	subnetID, err := GetSubnetID(ctx, client, env.VPCID, az)
	if err != nil {
		return fmt.Errorf("subnet lookup: %w", err)
	}

	volumeID, attachedInstanceID, err := GetVolume(ctx, client, sessionID, az)
	if err != nil {
		return fmt.Errorf("volume lookup: %w", err)
	}

	eniID, err := GetENIID(ctx, client, sessionID, az)
	if err != nil {
		return fmt.Errorf("eni lookup: %w", err)
	}

	if attachedInstanceID != "" {
		d, err := describeInstance(ctx, client, attachedInstanceID)
		if err != nil {
			return fmt.Errorf("describe instance: %w", err)
		}
		printInstance(ctx, d)
	} else {
		progress.Logf(ctx, "Instance: %s\n", notFound)
	}

	progress.Logf(ctx, "Subnet: %s\n", orNotFound(subnetID))
	progress.Logf(ctx, "Volume: %s\n", orNotFound(volumeID))
	progress.Logf(ctx, "Network: %s\n", orNotFound(eniID))
	return nil
}

func orNotFound(s string) string {
	if s == "" {
		return notFound
	}
	return s
}
