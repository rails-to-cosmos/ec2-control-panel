package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const notFound = "Not found"

func runStatus(ctx context.Context, env *EnvConfig, sessionID, az string) error {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(env.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	client := ec2.NewFromConfig(awsCfg)

	fmt.Printf("Session ID: %s\n", sessionID)
	fmt.Printf("VPC: %s\n", env.VPCID)
	fmt.Printf("Region: %s\n", env.Region)
	fmt.Printf("Availability zone: %s\n", az)

	subnetID, err := getSubnetID(ctx, client, env.VPCID, az)
	if err != nil {
		return fmt.Errorf("subnet lookup: %w", err)
	}

	volumeID, attachedInstanceID, err := getVolume(ctx, client, sessionID, az)
	if err != nil {
		return fmt.Errorf("volume lookup: %w", err)
	}

	eniID, err := getENIID(ctx, client, sessionID, az)
	if err != nil {
		return fmt.Errorf("eni lookup: %w", err)
	}

	if attachedInstanceID != "" {
		d, err := describeInstance(ctx, client, attachedInstanceID)
		if err != nil {
			return fmt.Errorf("describe instance: %w", err)
		}
		printInstance(d)
	} else {
		fmt.Printf("Instance: %s\n", notFound)
	}

	fmt.Printf("Subnet: %s\n", orNotFound(subnetID))
	fmt.Printf("Volume: %s\n", orNotFound(volumeID))
	fmt.Printf("Network: %s\n", orNotFound(eniID))
	return nil
}

func orNotFound(s string) string {
	if s == "" {
		return notFound
	}
	return s
}

func getSubnetID(ctx context.Context, c *ec2.Client, vpcID, az string) (string, error) {
	out, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
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

func getVolume(ctx context.Context, c *ec2.Client, name, az string) (volumeID, attachedInstanceID string, err error) {
	out, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
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

func getENIID(ctx context.Context, c *ec2.Client, name, az string) (string, error) {
	out, err := c.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
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

func describeInstance(ctx context.Context, c *ec2.Client, id string) (*InstanceDetails, error) {
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
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

	if typeOut, err := c.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
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

	if statusOut, err := c.DescribeInstanceStatus(ctx, &ec2.DescribeInstanceStatusInput{
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

func printInstance(d *InstanceDetails) {
	kind := "OnDemand"
	if d.Lifecycle == "spot" {
		kind = "Spot"
	}
	fmt.Printf("Instance: %s(%s)\n", kind, d.ID)
	fmt.Printf("    InstanceType: %s\n", d.InstanceType)
	fmt.Printf("    vCPUs: %d\n", d.VCpus)
	fmt.Printf("    Memory: %d MiB\n", d.MemoryMiB)
	fmt.Printf("    IP: %s\n", d.PrivateIP)
	fmt.Printf("    SSH: ssh ubuntu@%s\n", d.PrivateIP)
	if d.State != "" {
		fmt.Printf("    Status: %s, instance: %s, system: %s\n", d.State, d.InstanceCheck, d.SystemCheck)
	}
}
