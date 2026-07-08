package ec2

import (
	"context"
	"fmt"

	"ec2cp/src/config"
	"ec2cp/src/progress"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
)

// IP looks up the running instance's private IP (resolved via the AWS Name
// tag = awsName) and prints it on a single line.
func IP(ctx context.Context, env *config.EnvConfig, sessionID, awsName, az string) error {
	client, err := NewClient(ctx, env.Region)
	if err != nil {
		return err
	}

	_, instanceID, err := GetVolume(ctx, client, awsName, az)
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
