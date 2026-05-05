package ec2

import (
	"context"
	"fmt"

	"ec2cp/internal/progress"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// NewClient builds an EC2 SDK client for the given region using the default
// credential chain. Centralizing this avoids the LoadDefaultConfig-per-handler
// pattern (which hits IMDS/STS/file I/O each time).
func NewClient(ctx context.Context, region string) (*awsec2.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return awsec2.NewFromConfig(cfg), nil
}

// FirstNonEmpty returns the first non-empty string in xs, or "" if all are empty.
func FirstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// ----- read-only lookups -----

func GetSubnetID(ctx context.Context, c *awsec2.Client, vpcID, az string) (string, error) {
	out, err := c.DescribeSubnets(ctx, &awsec2.DescribeSubnetsInput{
		Filters: []types.Filter{
			{Name: aws.String("availability-zone"), Values: []string{az}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return "", err
	}
	if len(out.Subnets) == 0 {
		return "", nil
	}
	return aws.ToString(out.Subnets[0].SubnetId), nil
}

// GetVolume looks up the persistent EBS volume tagged Name=<name> in az.
// Returns "" for both fields if not found. attachedInstanceID is non-empty
// only when the volume is currently attached.
func GetVolume(ctx context.Context, c *awsec2.Client, name, az string) (volumeID, attachedInstanceID string, err error) {
	out, err := c.DescribeVolumes(ctx, &awsec2.DescribeVolumesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag-key"), Values: []string{"Name"}},
			{Name: aws.String("tag-value"), Values: []string{name}},
			{Name: aws.String("availability-zone"), Values: []string{az}},
		},
	})
	if err != nil {
		return "", "", err
	}
	if len(out.Volumes) == 0 {
		return "", "", nil
	}
	v := out.Volumes[0]
	volumeID = aws.ToString(v.VolumeId)
	if len(v.Attachments) > 0 {
		attachedInstanceID = aws.ToString(v.Attachments[0].InstanceId)
	}
	return volumeID, attachedInstanceID, nil
}

func GetENIID(ctx context.Context, c *awsec2.Client, name, az string) (string, error) {
	out, err := c.DescribeNetworkInterfaces(ctx, &awsec2.DescribeNetworkInterfacesInput{
		Filters: []types.Filter{
			{Name: aws.String("availability-zone"), Values: []string{az}},
			{Name: aws.String("tag-key"), Values: []string{"Name"}},
			{Name: aws.String("tag-value"), Values: []string{name}},
		},
	})
	if err != nil {
		return "", err
	}
	if len(out.NetworkInterfaces) == 0 {
		return "", nil
	}
	return aws.ToString(out.NetworkInterfaces[0].NetworkInterfaceId), nil
}

// ----- ENI mutation -----

func createENI(ctx context.Context, c *awsec2.Client, name, subnetID, sgID string) (string, error) {
	out, err := c.CreateNetworkInterface(ctx, &awsec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetID),
		Groups:   []string{sgID},
	})
	if err != nil {
		return "", fmt.Errorf("create-network-interface: %w", err)
	}
	eniID := aws.ToString(out.NetworkInterface.NetworkInterfaceId)

	if _, err := c.CreateTags(ctx, &awsec2.CreateTagsInput{
		Resources: []string{eniID},
		Tags: []types.Tag{
			{Key: aws.String("Name"), Value: aws.String(name)},
		},
	}); err != nil {
		return "", fmt.Errorf("tag eni %s: %w", eniID, err)
	}
	return eniID, nil
}

func GetOrCreateENI(ctx context.Context, c *awsec2.Client, name, subnetID, sgID, az string) (string, error) {
	if existing, err := GetENIID(ctx, c, name, az); err != nil {
		return "", err
	} else if existing != "" {
		return existing, nil
	}
	progress.Logf(ctx, "Creating network interface for %q\n", name)
	return createENI(ctx, c, name, subnetID, sgID)
}

// ----- instance details -----

type InstanceDetails struct {
	ID            string
	InstanceType  string
	PrivateIP     string
	Lifecycle     string
	VCpus         int32
	MemoryMiB     int64
	State         string
	InstanceCheck string
	SystemCheck   string
}

func describeInstance(ctx context.Context, c *awsec2.Client, id string) (*InstanceDetails, error) {
	out, err := c.DescribeInstances(ctx, &awsec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return nil, err
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("instance %s not found", id)
	}
	inst := out.Reservations[0].Instances[0]

	d := &InstanceDetails{
		ID:           aws.ToString(inst.InstanceId),
		InstanceType: string(inst.InstanceType),
		PrivateIP:    aws.ToString(inst.PrivateIpAddress),
		Lifecycle:    string(inst.InstanceLifecycle),
	}
	if d.Lifecycle == "" {
		d.Lifecycle = "ondemand"
	}

	if typeOut, err := c.DescribeInstanceTypes(ctx, &awsec2.DescribeInstanceTypesInput{
		InstanceTypes: []types.InstanceType{inst.InstanceType},
	}); err == nil && len(typeOut.InstanceTypes) > 0 {
		t := typeOut.InstanceTypes[0]
		if t.VCpuInfo != nil {
			d.VCpus = aws.ToInt32(t.VCpuInfo.DefaultVCpus)
		}
		if t.MemoryInfo != nil {
			d.MemoryMiB = aws.ToInt64(t.MemoryInfo.SizeInMiB)
		}
	}

	if statusOut, err := c.DescribeInstanceStatus(ctx, &awsec2.DescribeInstanceStatusInput{
		InstanceIds: []string{id},
	}); err == nil && len(statusOut.InstanceStatuses) > 0 {
		s := statusOut.InstanceStatuses[0]
		if s.InstanceState != nil {
			d.State = string(s.InstanceState.Name)
		}
		if s.InstanceStatus != nil {
			d.InstanceCheck = string(s.InstanceStatus.Status)
		}
		if s.SystemStatus != nil {
			d.SystemCheck = string(s.SystemStatus.Status)
		}
	}

	return d, nil
}

func printInstance(ctx context.Context, d *InstanceDetails) {
	kind := "OnDemand"
	if d.Lifecycle == "spot" {
		kind = "Spot"
	}
	progress.Logf(ctx, "Instance: %s(%s)\n", kind, d.ID)
	progress.Logf(ctx, "    InstanceType: %s\n", d.InstanceType)
	progress.Logf(ctx, "    vCPUs: %d\n", d.VCpus)
	progress.Logf(ctx, "    Memory: %d MiB\n", d.MemoryMiB)
	progress.Logf(ctx, "    IP: %s\n", d.PrivateIP)
	progress.Logf(ctx, "    SSH: ssh ubuntu@%s\n", d.PrivateIP)
	if d.State != "" {
		progress.Logf(ctx, "    Status: %s, instance: %s, system: %s\n", d.State, d.InstanceCheck, d.SystemCheck)
	}
}

// findInstancesByName returns the IDs of non-terminated instances tagged
// Name=<name> in the AZ. Used as a fallback when the persistent volume isn't
// attached (orphan recovery).
func findInstancesByName(ctx context.Context, c *awsec2.Client, name, az string) ([]string, error) {
	out, err := c.DescribeInstances(ctx, &awsec2.DescribeInstancesInput{
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

func getSpotRequestID(ctx context.Context, c *awsec2.Client, instanceID string) (string, error) {
	out, err := c.DescribeInstances(ctx, &awsec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
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
