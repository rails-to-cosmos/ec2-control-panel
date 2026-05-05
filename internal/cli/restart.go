package cli

import (
	"fmt"

	"ec2cp/internal/config"
	"ec2cp/internal/ec2"

	"github.com/spf13/cobra"
)

func restartCmd() *cobra.Command {
	var (
		yes              bool
		instanceType     string
		requestType      string
		instanceName     string
		availabilityZone string
		bidPriceFlag     string
	)
	cmd := &cobra.Command{
		Use:   "restart <session-id>",
		Short: "Restart existing instance, optionally changing instance type",
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
			if err := ConfirmDestructive(sessionID, "restart", yes); err != nil {
				return err
			}

			az := ec2.FirstNonEmpty(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone)
			awsName := inst.AWSName(sessionID)
			if err := ec2.Stop(cmd.Context(), env, sessionID, awsName, az, false, true, nil); err != nil {
				return fmt.Errorf("stop phase: %w", err)
			}

			rType, rTypeSrc := ec2.ResolveSource(requestType, inst.RequestType, env.DefaultRequestType,
				"request-type", "request_type", "EC2_REQUEST_TYPE")
			iType, iTypeSrc := ec2.ResolveSource(instanceType, inst.InstanceType, env.DefaultInstanceType,
				"instance-type", "instance_type", "EC2_INSTANCE_TYPE")
			_, azSrc := ec2.ResolveSource(availabilityZone, inst.AvailabilityZone, env.AvailabilityZone,
				"availability-zone", "availability_zone", "EC2_AVAILABILITY_ZONE")
			bidPrice, bidPriceSrc := ec2.ResolveSource(bidPriceFlag, "", env.BidPrice,
				"bid-price", "", "EC2_SPOT_BID_PRICE")

			name, nameSrc := instanceName, "--instance-name"
			if name == "" {
				name, nameSrc = awsName, "default"
			}

			return ec2.Start(cmd.Context(), ec2.LaunchParams{
				SessionID:          sessionID,
				AWSName:            awsName,
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
