package ec2

import (
	"context"
	"fmt"

	"ec2cp/internal/config"
	"ec2cp/internal/progress"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
)

// IP looks up the running instance's private IP and prints it (one line).
func IP(ctx context.Context, env *config.EnvConfig, sessionID, az string) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	client := awsec2.NewFromConfig(awsCfg)

	_, instanceID, err := GetVolume(ctx, client, sessionID, az)
	if err != nil {
		return fmt.Errorf("volume lookup: %w", err)
	}
	if instanceID == "" {
		return fmt.Errorf("no running instance for %q", sessionID)
	}
	out, err := client.DescribeInstances(ctx, &awsec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("describe-instances: %w", err)
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return fmt.Errorf("instance %s vanished", instanceID)
	}
	progress.Logf(ctx, "%s\n", aws.ToString(out.Reservations[0].Instances[0].PrivateIpAddress))
	return nil
}
