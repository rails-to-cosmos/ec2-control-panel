package cli

import (
	"fmt"

	"ec2cp/src/config"
	"ec2cp/src/ec2"

	"github.com/spf13/cobra"
)

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
			env, err := config.LoadEnv()
			if err != nil {
				return err
			}
			if err := env.RequireForLaunch(); err != nil {
				return err
			}
			inst, err := config.GetInstance(sessionID)
			if err != nil {
				return err
			}
			if err := ConfirmDestructive(sessionID, "start", yes); err != nil {
				return err
			}

			az, azSrc := ec2.ResolveSource(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone,
				"availability-zone", "availability_zone", "EC2_AVAILABILITY_ZONE")
			rType, rTypeSrc := ec2.ResolveSource(requestType, inst.RequestType, env.DefaultRequestType,
				"request-type", "request_type", "EC2_REQUEST_TYPE")
			if rType != "spot" && rType != "ondemand" {
				return fmt.Errorf("invalid request type %q (spot|ondemand)", rType)
			}
			iType, iTypeSrc := ec2.ResolveSource(instanceType, inst.InstanceType, env.DefaultInstanceType,
				"instance-type", "instance_type", "EC2_INSTANCE_TYPE")
			bidPrice, bidPriceSrc := ec2.ResolveSource(bidPriceFlag, "", env.BidPrice,
				"bid-price", "", "EC2_SPOT_BID_PRICE")

			persistentVol, persistentVolSrc := ec2.ResolvePersistentVolumeSize(inst.VolumeSize, env.DefaultVolumeSize)
			awsName := inst.AWSName(sessionID)
			name, nameSrc := instanceName, "--instance-name"
			if name == "" {
				name, nameSrc = awsName, "default"
			}

			return ec2.Start(cmd.Context(), ec2.LaunchParams{
				SessionID:                  sessionID,
				AWSName:                    awsName,
				Owner:                      inst.Owner,
				InstanceName:               name,
				InstanceType:               iType,
				RequestType:                rType,
				VolumeSize:                 env.InstanceVolumeSize,
				PersistentVolumeSize:       persistentVol,
				PersistentVolumeSizeSource: persistentVolSrc,
				Env:                        env,
				AZ:                         az,
				BidPrice:                   bidPrice,
				InstanceNameSource:         nameSrc,
				InstanceTypeSource:         iTypeSrc,
				RequestTypeSource:          rTypeSrc,
				AZSource:                   azSrc,
				BidPriceSource:             bidPriceSrc,
			})
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
