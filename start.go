package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
)

const launchWaitDuration = 15 * time.Minute

type LaunchParams struct {
	SessionID    string
	InstanceName string
	InstanceType string
	RequestType  string
	VolumeSize   int // root volume size for the actual instance
	Env          *EnvConfig
	AZ           string
	BidPrice     string

	// Source annotations for the report (e.g. "--instance-type", "instances.json:instance_type", "env:EC2_INSTANCE_TYPE")
	InstanceNameSource string
	InstanceTypeSource string
	RequestTypeSource  string
	AZSource           string
	BidPriceSource     string
}

// resolveSource returns the first non-empty of (flag, override, def) along with a label
// describing where it came from. Used to annotate the launch report so users can see
// which override won.
func resolveSource(flag, override, def, flagName, overrideName, defName string) (value, source string) {
	switch {
	case flag != "":
		return flag, "--" + flagName
	case override != "":
		return override, "instances.json:" + overrideName
	default:
		return def, "env:" + defName
	}
}

func startCmd() *cobra.Command {
	var (
		yes              bool
		instanceType     string
		requestType      string
		instanceName     string
		availabilityZone string
		bidPriceFlag     string
	)
	cmd := &cobra.Command{
		Use:   "start <session-id>",
		Short: "Start your lovely instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			env, err := loadEnvConfig()
			if err != nil {
				return err
			}
			if err := env.requireForLaunch(); err != nil {
				return err
			}
			inst, err := getInstanceConfig(sessionID)
			if err != nil {
				return err
			}
			if err := confirmDestructive(sessionID, "start", yes); err != nil {
				return err
			}

			az, azSrc := resolveSource(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone,
				"availability-zone", "availability_zone", "EC2_AVAILABILITY_ZONE")
			rType, rTypeSrc := resolveSource(requestType, inst.RequestType, env.DefaultRequestType,
				"request-type", "request_type", "EC2_REQUEST_TYPE")
			if rType != "spot" && rType != "ondemand" {
				return fmt.Errorf("invalid request type %q (spot|ondemand)", rType)
			}
			iType, iTypeSrc := resolveSource(instanceType, inst.InstanceType, env.DefaultInstanceType,
				"instance-type", "instance_type", "EC2_INSTANCE_TYPE")
			bidPrice, bidPriceSrc := resolveSource(bidPriceFlag, "", env.BidPrice,
				"bid-price", "", "EC2_SPOT_BID_PRICE")

			name, nameSrc := instanceName, "--instance-name"
			if name == "" {
				name, nameSrc = sessionID, "session-id default"
			}

			params := LaunchParams{
				SessionID:          sessionID,
				InstanceName:       name,
				InstanceType:       iType,
				RequestType:        rType,
				VolumeSize:         env.InstanceVolumeSize,
				Env:                env,
				AZ:                 az,
				BidPrice:           bidPrice,
				InstanceNameSource: nameSrc,
				InstanceTypeSource: iTypeSrc,
				RequestTypeSource:  rTypeSrc,
				AZSource:           azSrc,
				BidPriceSource:     bidPriceSrc,
			}
			return runStart(cmd.Context(), params)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().StringVar(&instanceType, "instance-type", "", "instance type (overrides config + env)")
	cmd.Flags().StringVar(&requestType, "request-type", "", "spot|ondemand (overrides config + env)")
	cmd.Flags().StringVar(&instanceName, "instance-name", "", "Name tag (defaults to session-id)")
	cmd.Flags().StringVarP(&availabilityZone, "availability-zone", "a", "", "AZ override (defaults to instance config or EC2_AVAILABILITY_ZONE)")
	cmd.Flags().StringVar(&bidPriceFlag, "bid-price", "", "max spot bid price USD/hour (overrides EC2_SPOT_BID_PRICE)")
	return cmd
}

func runStart(ctx context.Context, p LaunchParams) error {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(p.Env.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	client := ec2.NewFromConfig(awsCfg)

	logf(ctx,"Session ID:        %s\n", p.SessionID)
	logf(ctx,"Instance name:     %s  (%s)\n", p.InstanceName, p.InstanceNameSource)
	logf(ctx,"Instance type:     %s  (%s)\n", p.InstanceType, p.InstanceTypeSource)
	logf(ctx,"Request type:      %s  (%s)\n", p.RequestType, p.RequestTypeSource)
	logf(ctx,"Region:            %s  (env:EC2_REGION)\n", p.Env.Region)
	logf(ctx,"Availability zone: %s  (%s)\n", p.AZ, p.AZSource)
	if p.RequestType == "spot" {
		logf(ctx,"Bid price:         %s  (%s)\n", p.BidPrice, p.BidPriceSource)
	}

	subnetID, err := getSubnetID(ctx, client, p.Env.VPCID, p.AZ)
	if err != nil {
		return fmt.Errorf("subnet lookup: %w", err)
	}
	if subnetID == "" {
		return fmt.Errorf("no subnet found for VPC %s in AZ %s", p.Env.VPCID, p.AZ)
	}

	eniID, err := getOrCreateENI(ctx, client, p.SessionID, subnetID, p.Env.SecurityGroup, p.AZ)
	if err != nil {
		return fmt.Errorf("eni: %w", err)
	}

	volumeID, attachedInstanceID, err := getVolume(ctx, client, p.SessionID, p.AZ)
	if err != nil {
		return fmt.Errorf("volume lookup: %w", err)
	}

	if attachedInstanceID != "" {
		logf(ctx,"Instance is already running: %s\n", attachedInstanceID)
		return nil
	}

	persistentVolumeID := volumeID
	if persistentVolumeID == "" {
		logf(ctx, "First start — launching temp spot to persist volume\n")
		persistentVolumeID, err = makePersistentVolume(ctx, client, p, eniID)
		if err != nil {
			return fmt.Errorf("persist volume: %w", err)
		}
	} else {
		logf(ctx,"Reusing persistent volume %s\n", persistentVolumeID)
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

	logf(ctx,"Waiting for instance %s to pass status checks\n", instanceID)
	statusWaiter := ec2.NewInstanceStatusOkWaiter(client)
	if err := statusWaiter.Wait(ctx, &ec2.DescribeInstanceStatusInput{
		InstanceIds: []string{instanceID},
	}, launchWaitDuration); err != nil {
		return fmt.Errorf("wait instance-status-ok: %w", err)
	}

	logf(ctx,"Waiting for chainload to attach volume %s\n", persistentVolumeID)
	inUseWaiter := ec2.NewVolumeInUseWaiter(client)
	if err := inUseWaiter.Wait(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{persistentVolumeID},
	}, launchWaitDuration); err != nil {
		return fmt.Errorf("volume %s never attached — chainload likely failed: %w", persistentVolumeID, err)
	}
	if err := verifyVolumeAttachedTo(ctx, client, persistentVolumeID, instanceID); err != nil {
		return err
	}

	logf(ctx,"\nInstance %q is ready: %s\n", p.SessionID, instanceID)
	return nil
}

func verifyVolumeAttachedTo(ctx context.Context, client *ec2.Client, volumeID, instanceID string) error {
	out, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
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

// makePersistentVolume launches a throwaway spot, marks its root volume non-deleting,
// tags it with the session id, and terminates the spot. The Volume ID returned is the
// persistent EBS volume the next launch will chainload from.
func makePersistentVolume(ctx context.Context, client *ec2.Client, p LaunchParams, eniID string) (string, error) {
	refUserData, err := referenceUserData()
	if err != nil {
		return "", err
	}

	spotID, requestID, err := submitSpotRequest(ctx, client, spotRequestParams{
		Name:           p.InstanceName,
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
	logf(ctx,"Temp spot %s (request %s) launched\n", spotID, requestID)

	// Always tear down the temp spot, even if the persist step fails.
	persistentID, persistErr := persistRootVolume(ctx, client, spotID, p.SessionID)
	if termErr := terminateSpot(ctx, client, spotID, requestID, ""); termErr != nil {
		if persistErr != nil {
			return "", fmt.Errorf("persist failed: %w; teardown also failed: %v", persistErr, termErr)
		}
		return "", fmt.Errorf("teardown of temp spot: %w", termErr)
	}
	if persistErr != nil {
		return "", persistErr
	}

	logf(ctx,"Waiting for persistent volume %s to become available\n", persistentID)
	volWaiter := ec2.NewVolumeAvailableWaiter(client)
	if err := volWaiter.Wait(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{persistentID},
	}, launchWaitDuration); err != nil {
		return "", fmt.Errorf("wait persistent volume available: %w", err)
	}
	return persistentID, nil
}

func persistRootVolume(ctx context.Context, client *ec2.Client, instanceID, persistentName string) (string, error) {
	if _, err := client.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
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

	desc, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
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

	if _, err := client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{volumeID},
		Tags: []types.Tag{
			{Key: aws.String("Name"), Value: aws.String(persistentName)},
		},
	}); err != nil {
		return "", fmt.Errorf("tag persistent volume: %w", err)
	}
	logf(ctx,"Persistent volume %s tagged %q\n", volumeID, persistentName)
	return volumeID, nil
}

type spotRequestParams struct {
	Name           string
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

// submitSpotRequest issues a request, waits for fulfillment, tags the instance,
// and waits for it to be running. Returns instance id + request id.
func submitSpotRequest(ctx context.Context, client *ec2.Client, p spotRequestParams) (instanceID, requestID string, err error) {
	out, err := client.RequestSpotInstances(ctx, &ec2.RequestSpotInstancesInput{
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
	logf(ctx,"Spot request: %s (waiting for fulfillment)\n", requestID)

	fulfilledWaiter := ec2.NewSpotInstanceRequestFulfilledWaiter(client)
	if err := fulfilledWaiter.Wait(ctx, &ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{requestID},
	}, launchWaitDuration); err != nil {
		detail := describeSpotFailure(ctx, client, requestID)
		logf(ctx,"Spot request %s did not fulfill: %s\n", requestID, detail)
		// Best-effort cleanup so the failed request doesn't linger.
		_, _ = client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{requestID},
		})
		return "", "", fmt.Errorf("spot request %s did not fulfill: %s", requestID, detail)
	}

	desc, err := client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{requestID},
	})
	if err != nil {
		return "", "", err
	}
	if len(desc.SpotInstanceRequests) == 0 {
		return "", "", fmt.Errorf("spot request %s vanished", requestID)
	}
	instanceID = aws.ToString(desc.SpotInstanceRequests[0].InstanceId)

	if _, err := client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags: []types.Tag{
			{Key: aws.String("Name"), Value: aws.String(p.Name)},
			{Key: aws.String("spot-request-id"), Value: aws.String(requestID)},
			{Key: aws.String("request-type"), Value: aws.String("spot")},
		},
	}); err != nil {
		return "", "", fmt.Errorf("tag instance: %w", err)
	}

	logf(ctx,"Spot instance %s — waiting for running state\n", instanceID)
	runningWaiter := ec2.NewInstanceRunningWaiter(client)
	if err := runningWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, launchWaitDuration); err != nil {
		return "", "", fmt.Errorf("wait instance-running: %w", err)
	}
	return instanceID, requestID, nil
}

func requestSpot(ctx context.Context, client *ec2.Client, p LaunchParams, eniID, userData string) (string, error) {
	id, _, err := submitSpotRequest(ctx, client, spotRequestParams{
		Name:           p.InstanceName,
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

func requestOnDemand(ctx context.Context, client *ec2.Client, p LaunchParams, eniID, userData string) (string, error) {
	out, err := client.RunInstances(ctx, &ec2.RunInstancesInput{
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

	if _, err := client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags: []types.Tag{
			{Key: aws.String("Name"), Value: aws.String(p.InstanceName)},
			{Key: aws.String("request-type"), Value: aws.String("ondemand")},
		},
	}); err != nil {
		return "", fmt.Errorf("tag instance: %w", err)
	}

	logf(ctx,"OnDemand instance %s — waiting for running state\n", instanceID)
	runningWaiter := ec2.NewInstanceRunningWaiter(client)
	if err := runningWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, launchWaitDuration); err != nil {
		return "", fmt.Errorf("wait instance-running: %w", err)
	}
	return instanceID, nil
}

// describeSpotFailure pulls the State/Status/Fault fields from the spot request so we can
// surface the actual reason (price-too-low, capacity-not-available, ...) instead of the
// waiter's opaque "Failure" message.
func describeSpotFailure(ctx context.Context, c *ec2.Client, requestID string) string {
	out, err := c.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
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

// terminateSpot is shared between the temp-spot teardown in start and the regular stop path.
// It cancels the spot request (if non-empty) and terminates the instance, then waits for the
// volume + ENI to detach.
func terminateSpot(ctx context.Context, client *ec2.Client, instanceID, requestID, eniID string) error {
	if requestID != "" {
		if _, err := client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{requestID},
		}); err != nil {
			return fmt.Errorf("cancel spot request: %w", err)
		}
	}
	if _, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		return fmt.Errorf("terminate-instances: %w", err)
	}
	termWaiter := ec2.NewInstanceTerminatedWaiter(client)
	if err := termWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, launchWaitDuration); err != nil {
		return fmt.Errorf("wait instance-terminated: %w", err)
	}
	if eniID != "" {
		eniWaiter := ec2.NewNetworkInterfaceAvailableWaiter(client)
		if err := eniWaiter.Wait(ctx, &ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: []string{eniID},
		}, launchWaitDuration); err != nil {
			return fmt.Errorf("wait eni-available: %w", err)
		}
	}
	return nil
}
