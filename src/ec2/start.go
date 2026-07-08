package ec2

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ec2cp/src/progress"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const launchWaitDuration = 15 * time.Minute

func Start(ctx context.Context, p LaunchParams) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(p.Env.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	client := awsec2.NewFromConfig(awsCfg)

	progress.Logf(ctx, "Session ID:        %s\n", p.SessionID)
	progress.Logf(ctx, "Instance name:     %s  (%s)\n", p.InstanceName, p.InstanceNameSource)
	progress.Logf(ctx, "Instance type:     %s  (%s)\n", p.InstanceType, p.InstanceTypeSource)
	progress.Logf(ctx, "Request type:      %s  (%s)\n", p.RequestType, p.RequestTypeSource)
	progress.Logf(ctx, "Region:            %s  (env:EC2_REGION)\n", p.Env.Region)
	progress.Logf(ctx, "Availability zone: %s  (%s)\n", p.AZ, p.AZSource)
	if p.RequestType == "spot" {
		progress.Logf(ctx, "Bid price:         %s  (%s)\n", p.BidPrice, p.BidPriceSource)
	}

	subnetID, err := GetSubnetID(ctx, client, p.Env.VPCID, p.AZ)
	if err != nil {
		return fmt.Errorf("subnet lookup: %w", err)
	}
	if subnetID == "" {
		return fmt.Errorf("no subnet found for VPC %s in AZ %s", p.Env.VPCID, p.AZ)
	}

	eniID, err := GetOrCreateENI(ctx, client, p.AWSName, subnetID, p.Env.SecurityGroup, p.AZ)
	if err != nil {
		return fmt.Errorf("eni: %w", err)
	}

	volumeID, attachedInstanceID, err := GetVolume(ctx, client, p.AWSName, p.AZ)
	if err != nil {
		return fmt.Errorf("volume lookup: %w", err)
	}

	if attachedInstanceID != "" {
		progress.Logf(ctx, "Instance is already running: %s\n", attachedInstanceID)
		return nil
	}

	persistentVolumeID := volumeID
	if persistentVolumeID == "" {
		progress.Logf(ctx, "First start — launching temp spot to persist volume\n")
		persistentVolumeID, err = makePersistentVolume(ctx, client, p, eniID)
		if err != nil {
			return fmt.Errorf("persist volume: %w", err)
		}
	} else {
		progress.Logf(ctx, "Reusing persistent volume %s\n", persistentVolumeID)
	}

	userData, err := chainloadUserData(persistentVolumeID, p.Env.AWSAccessKeyID, p.Env.AWSSecretAccessKey, p.Env.Region)
	if err != nil {
		return fmt.Errorf("chainload userdata: %w", err)
	}

	var instanceID string
	switch p.RequestType {
	case "ondemand":
		instanceID, err = requestOnDemand(ctx, client, p, eniID, userData)
	case "spot":
		instanceID, err = requestSpot(ctx, client, p, eniID, userData)
	}
	if err != nil {
		return fmt.Errorf("%s request: %w", p.RequestType, err)
	}

	progress.Logf(ctx, "Waiting for instance %s to pass status checks\n", instanceID)
	statusWaiter := awsec2.NewInstanceStatusOkWaiter(client)
	if err := statusWaiter.Wait(ctx, &awsec2.DescribeInstanceStatusInput{
		InstanceIds: []string{instanceID},
	}, launchWaitDuration); err != nil {
		return fmt.Errorf("wait instance-status-ok: %w", err)
	}

	progress.Logf(ctx, "Waiting for chainload to attach volume %s\n", persistentVolumeID)
	inUseWaiter := awsec2.NewVolumeInUseWaiter(client)
	if err := inUseWaiter.Wait(ctx, &awsec2.DescribeVolumesInput{
		VolumeIds: []string{persistentVolumeID},
	}, launchWaitDuration); err != nil {
		return fmt.Errorf("volume %s never attached — chainload likely failed: %w", persistentVolumeID, err)
	}
	if err := verifyVolumeAttachedTo(ctx, client, persistentVolumeID, instanceID); err != nil {
		return err
	}

	progress.Logf(ctx, "\nInstance %q is ready: %s\n", p.SessionID, instanceID)
	return nil
}

func verifyVolumeAttachedTo(ctx context.Context, client *awsec2.Client, volumeID, instanceID string) error {
	out, err := client.DescribeVolumes(ctx, &awsec2.DescribeVolumesInput{
		VolumeIds: []string{volumeID},
	})
	if err != nil {
		return fmt.Errorf("verify volume attachment: %w", err)
	}
	if len(out.Volumes) == 0 || len(out.Volumes[0].Attachments) == 0 {
		return fmt.Errorf("volume %s in-use but has no attachments", volumeID)
	}
	attached := aws.ToString(out.Volumes[0].Attachments[0].InstanceId)
	if attached != instanceID {
		return fmt.Errorf("volume %s is attached to %s, not our instance %s", volumeID, attached, instanceID)
	}
	return nil
}

// makePersistentVolume launches a throwaway spot, marks its root volume
// non-deleting, tags it with the session id, and terminates the spot. The
// returned Volume ID is the persistent EBS volume the next launch chainloads from.
func makePersistentVolume(ctx context.Context, client *awsec2.Client, p LaunchParams, eniID string) (string, error) {
	refUserData, err := referenceUserData()
	if err != nil {
		return "", err
	}

	spotID, requestID, err := submitSpotRequest(ctx, client, spotRequestParams{
		Name:           p.InstanceName,
		Owner:          p.Owner,
		AMIID:          p.Env.AMIID,
		InstanceType:   p.InstanceType,
		KeyName:        p.Env.PublicKey,
		IamRoleArn:     p.Env.InstanceRole,
		ENIID:          eniID,
		AZ:             p.AZ,
		VolumeSize:     int32(p.Env.DefaultVolumeSize),
		UserDataBase64: refUserData,
		BidPrice:       p.BidPrice,
	})
	if err != nil {
		return "", fmt.Errorf("temp spot request: %w", err)
	}
	progress.Logf(ctx, "Temp spot %s (request %s) launched\n", spotID, requestID)

	persistentID, persistErr := persistRootVolume(ctx, client, spotID, p.AWSName, p.Owner)
	if termErr := terminateSpot(ctx, client, spotID, requestID, ""); termErr != nil {
		if persistErr != nil {
			return "", fmt.Errorf("persist failed: %w; teardown also failed: %v", persistErr, termErr)
		}
		return "", fmt.Errorf("teardown of temp spot: %w", termErr)
	}
	if persistErr != nil {
		return "", persistErr
	}

	progress.Logf(ctx, "Waiting for persistent volume %s to become available\n", persistentID)
	volWaiter := awsec2.NewVolumeAvailableWaiter(client)
	if err := volWaiter.Wait(ctx, &awsec2.DescribeVolumesInput{
		VolumeIds: []string{persistentID},
	}, launchWaitDuration); err != nil {
		return "", fmt.Errorf("wait persistent volume available: %w", err)
	}
	return persistentID, nil
}

func persistRootVolume(ctx context.Context, client *awsec2.Client, instanceID, persistentName, owner string) (string, error) {
	if _, err := client.ModifyInstanceAttribute(ctx, &awsec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		BlockDeviceMappings: []types.InstanceBlockDeviceMappingSpecification{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &types.EbsInstanceBlockDeviceSpecification{
					DeleteOnTermination: aws.Bool(false),
				},
			},
		},
	}); err != nil {
		return "", fmt.Errorf("modify-instance-attribute: %w", err)
	}

	desc, err := client.DescribeInstances(ctx, &awsec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return "", fmt.Errorf("describe-instances: %w", err)
	}
	if len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance %s vanished", instanceID)
	}
	mappings := desc.Reservations[0].Instances[0].BlockDeviceMappings
	if len(mappings) == 0 || mappings[0].Ebs == nil {
		return "", fmt.Errorf("no root EBS on instance %s", instanceID)
	}
	volumeID := aws.ToString(mappings[0].Ebs.VolumeId)

	if _, err := client.CreateTags(ctx, &awsec2.CreateTagsInput{
		Resources: []string{volumeID},
		Tags: tagsWithOwner([]types.Tag{
			{Key: aws.String("Name"), Value: aws.String(persistentName)},
		}, owner),
	}); err != nil {
		return "", fmt.Errorf("tag persistent volume: %w", err)
	}
	progress.Logf(ctx, "Persistent volume %s tagged %q\n", volumeID, persistentName)
	return volumeID, nil
}

type spotRequestParams struct {
	Name           string
	Owner          string // optional; tagged on the launched instance when set
	AMIID          string
	InstanceType   string
	KeyName        string
	IamRoleArn     string
	ENIID          string
	AZ             string
	VolumeSize     int32
	UserDataBase64 string
	BidPrice       string
}

func submitSpotRequest(ctx context.Context, client *awsec2.Client, p spotRequestParams) (instanceID, requestID string, err error) {
	out, err := client.RequestSpotInstances(ctx, &awsec2.RequestSpotInstancesInput{
		SpotPrice: aws.String(p.BidPrice),
		LaunchSpecification: &types.RequestSpotLaunchSpecification{
			ImageId:      aws.String(p.AMIID),
			InstanceType: types.InstanceType(p.InstanceType),
			KeyName:      aws.String(p.KeyName),
			EbsOptimized: aws.Bool(true),
			Placement:    &types.SpotPlacement{AvailabilityZone: aws.String(p.AZ)},
			IamInstanceProfile: &types.IamInstanceProfileSpecification{
				Arn: aws.String(p.IamRoleArn),
			},
			BlockDeviceMappings: []types.BlockDeviceMapping{{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &types.EbsBlockDevice{
					DeleteOnTermination: aws.Bool(true),
					VolumeType:          types.VolumeTypeGp3,
					VolumeSize:          aws.Int32(p.VolumeSize),
				},
			}},
			NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
				DeviceIndex:        aws.Int32(0),
				NetworkInterfaceId: aws.String(p.ENIID),
			}},
			UserData: aws.String(p.UserDataBase64),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("request-spot-instances: %w", err)
	}
	if len(out.SpotInstanceRequests) == 0 {
		return "", "", fmt.Errorf("no spot request returned")
	}
	requestID = aws.ToString(out.SpotInstanceRequests[0].SpotInstanceRequestId)
	progress.Logf(ctx, "Spot request: %s (waiting for fulfillment)\n", requestID)

	fulfilledWaiter := awsec2.NewSpotInstanceRequestFulfilledWaiter(client)
	if err := fulfilledWaiter.Wait(ctx, &awsec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{requestID},
	}, launchWaitDuration); err != nil {
		detail := describeSpotFailure(ctx, client, requestID)
		progress.Logf(ctx, "Spot request %s did not fulfill: %s\n", requestID, detail)
		_, _ = client.CancelSpotInstanceRequests(ctx, &awsec2.CancelSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{requestID},
		})
		return "", "", fmt.Errorf("spot request %s did not fulfill: %s", requestID, detail)
	}

	desc, err := client.DescribeSpotInstanceRequests(ctx, &awsec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{requestID},
	})
	if err != nil {
		return "", "", err
	}
	if len(desc.SpotInstanceRequests) == 0 {
		return "", "", fmt.Errorf("spot request %s vanished", requestID)
	}
	instanceID = aws.ToString(desc.SpotInstanceRequests[0].InstanceId)

	if _, err := client.CreateTags(ctx, &awsec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags: tagsWithOwner([]types.Tag{
			{Key: aws.String("Name"), Value: aws.String(p.Name)},
			{Key: aws.String("spot-request-id"), Value: aws.String(requestID)},
			{Key: aws.String("request-type"), Value: aws.String("spot")},
		}, p.Owner),
	}); err != nil {
		return "", "", fmt.Errorf("tag instance: %w", err)
	}

	progress.Logf(ctx, "Spot instance %s — waiting for running state\n", instanceID)
	runningWaiter := awsec2.NewInstanceRunningWaiter(client)
	if err := runningWaiter.Wait(ctx, &awsec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, launchWaitDuration); err != nil {
		return "", "", fmt.Errorf("wait instance-running: %w", err)
	}
	return instanceID, requestID, nil
}

func requestSpot(ctx context.Context, client *awsec2.Client, p LaunchParams, eniID, userData string) (string, error) {
	id, _, err := submitSpotRequest(ctx, client, spotRequestParams{
		Name:           p.InstanceName,
		Owner:          p.Owner,
		AMIID:          p.Env.AMIID,
		InstanceType:   p.InstanceType,
		KeyName:        p.Env.PublicKey,
		IamRoleArn:     p.Env.InstanceRole,
		ENIID:          eniID,
		AZ:             p.AZ,
		VolumeSize:     int32(p.VolumeSize),
		UserDataBase64: userData,
		BidPrice:       p.BidPrice,
	})
	return id, err
}

func requestOnDemand(ctx context.Context, client *awsec2.Client, p LaunchParams, eniID, userData string) (string, error) {
	out, err := client.RunInstances(ctx, &awsec2.RunInstancesInput{
		ImageId:      aws.String(p.Env.AMIID),
		InstanceType: types.InstanceType(p.InstanceType),
		KeyName:      aws.String(p.Env.PublicKey),
		EbsOptimized: aws.Bool(true),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		Placement:    &types.Placement{AvailabilityZone: aws.String(p.AZ)},
		IamInstanceProfile: &types.IamInstanceProfileSpecification{
			Arn: aws.String(p.Env.InstanceRole),
		},
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"),
			Ebs: &types.EbsBlockDevice{
				DeleteOnTermination: aws.Bool(true),
				VolumeType:          types.VolumeTypeGp3,
				VolumeSize:          aws.Int32(int32(p.VolumeSize)),
			},
		}},
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:        aws.Int32(0),
			NetworkInterfaceId: aws.String(eniID),
		}},
		UserData: aws.String(userData),
	})
	if err != nil {
		return "", fmt.Errorf("run-instances: %w", err)
	}
	if len(out.Instances) == 0 {
		return "", fmt.Errorf("no instance returned")
	}
	instanceID := aws.ToString(out.Instances[0].InstanceId)

	if _, err := client.CreateTags(ctx, &awsec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags: tagsWithOwner([]types.Tag{
			{Key: aws.String("Name"), Value: aws.String(p.InstanceName)},
			{Key: aws.String("request-type"), Value: aws.String("ondemand")},
		}, p.Owner),
	}); err != nil {
		return "", fmt.Errorf("tag instance: %w", err)
	}

	progress.Logf(ctx, "OnDemand instance %s — waiting for running state\n", instanceID)
	runningWaiter := awsec2.NewInstanceRunningWaiter(client)
	if err := runningWaiter.Wait(ctx, &awsec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, launchWaitDuration); err != nil {
		return "", fmt.Errorf("wait instance-running: %w", err)
	}
	return instanceID, nil
}

// describeSpotFailure pulls the State/Status/Fault fields from the spot request
// so we can surface the actual reason instead of the waiter's opaque "Failure".
func describeSpotFailure(ctx context.Context, c *awsec2.Client, requestID string) string {
	out, err := c.DescribeSpotInstanceRequests(ctx, &awsec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{requestID},
	})
	if err != nil {
		return fmt.Sprintf("(could not fetch spot request details: %v)", err)
	}
	if len(out.SpotInstanceRequests) == 0 {
		return "(spot request not found)"
	}
	r := out.SpotInstanceRequests[0]
	parts := []string{fmt.Sprintf("state=%s", r.State)}
	if r.Status != nil {
		if code := aws.ToString(r.Status.Code); code != "" {
			parts = append(parts, fmt.Sprintf("status=%s", code))
		}
		if msg := aws.ToString(r.Status.Message); msg != "" {
			parts = append(parts, fmt.Sprintf("message=%q", msg))
		}
	}
	if r.Fault != nil {
		if code := aws.ToString(r.Fault.Code); code != "" {
			parts = append(parts, fmt.Sprintf("fault=%s", code))
		}
		if msg := aws.ToString(r.Fault.Message); msg != "" {
			parts = append(parts, fmt.Sprintf("fault_message=%q", msg))
		}
	}
	return strings.Join(parts, " ")
}

// tagsWithOwner appends an Owner tag to base if owner is non-empty.
func tagsWithOwner(base []types.Tag, owner string) []types.Tag {
	if owner == "" {
		return base
	}
	return append(base, types.Tag{Key: aws.String("Owner"), Value: aws.String(owner)})
}

// terminateSpot is used for the temp-spot teardown in start. It cancels the
// spot request, terminates the instance, then waits for it to be terminated
// and (optionally) for the ENI to detach.
func terminateSpot(ctx context.Context, client *awsec2.Client, instanceID, requestID, eniID string) error {
	if requestID != "" {
		if _, err := client.CancelSpotInstanceRequests(ctx, &awsec2.CancelSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{requestID},
		}); err != nil {
			return fmt.Errorf("cancel spot request: %w", err)
		}
	}
	if _, err := client.TerminateInstances(ctx, &awsec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		return fmt.Errorf("terminate-instances: %w", err)
	}
	termWaiter := awsec2.NewInstanceTerminatedWaiter(client)
	if err := termWaiter.Wait(ctx, &awsec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, launchWaitDuration); err != nil {
		return fmt.Errorf("wait instance-terminated: %w", err)
	}
	if eniID != "" {
		eniWaiter := awsec2.NewNetworkInterfaceAvailableWaiter(client)
		if err := eniWaiter.Wait(ctx, &awsec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: []string{eniID},
		}, launchWaitDuration); err != nil {
			return fmt.Errorf("wait eni-available: %w", err)
		}
	}
	return nil
}
