package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func createENI(ctx context.Context, c *ec2.Client, name, subnetID, sgID string) (string, error) {
	out, err := c.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetID),
		Groups:   []string{sgID},
	})
	if err != nil {
		return "", fmt.Errorf("create-network-interface: %w", err)
	}
	eniID := aws.ToString(out.NetworkInterface.NetworkInterfaceId)

	if _, err := c.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{eniID},
		Tags: []types.Tag{
			{Key: aws.String("Name"), Value: aws.String(name)},
		},
	}); err != nil {
		return "", fmt.Errorf("tag eni %s: %w", eniID, err)
	}
	return eniID, nil
}

func getOrCreateENI(ctx context.Context, c *ec2.Client, name, subnetID, sgID, az string) (string, error) {
	if existing, err := getENIID(ctx, c, name, az); err != nil {
		return "", err
	} else if existing != "" {
		return existing, nil
	}
	logf(ctx,"Creating network interface for %q\n", name)
	return createENI(ctx, c, name, subnetID, sgID)
}
