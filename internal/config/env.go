// Package config loads environment-wide defaults from .env and per-instance
// overrides from instances.json. EnvConfig is the runtime-required subset;
// LoadEnv fails if any required key is missing.
package config

import (
	"fmt"
	"os"
	"strconv"
)

type EnvConfig struct {
	// Required for all commands.
	Region           string
	AvailabilityZone string
	VPCID            string
	SecurityGroup    string

	// Required only for launch (start/restart).
	AMIID               string
	PublicKey           string
	InstanceRole        string
	DefaultInstanceType string
	DefaultRequestType  string
	DefaultVolumeSize   int
	InstanceVolumeSize  int
	BidPrice            string

	// AWS creds are forwarded into the chainload userdata script.
	AWSAccessKeyID     string
	AWSSecretAccessKey string
}

func LoadEnv() (*EnvConfig, error) {
	c := &EnvConfig{
		Region:              os.Getenv("EC2_REGION"),
		AvailabilityZone:    os.Getenv("EC2_AVAILABILITY_ZONE"),
		VPCID:               os.Getenv("EC2_VPC_ID"),
		SecurityGroup:       os.Getenv("EC2_SECURITY_GROUP"),
		AMIID:               os.Getenv("EC2_AMI_ID"),
		PublicKey:           os.Getenv("EC2_PUBLIC_KEY"),
		InstanceRole:        os.Getenv("EC2_ROLE"),
		DefaultInstanceType: getenvDefault("EC2_INSTANCE_TYPE", "r7i.2xlarge"),
		DefaultRequestType:  getenvDefault("EC2_REQUEST_TYPE", "spot"),
		DefaultVolumeSize:   getenvInt("EC2_VOLUME_SIZE", 512),
		InstanceVolumeSize:  getenvInt("EC2_INSTANCE_VOLUME_SIZE", 30),
		BidPrice:            getenvDefault("EC2_SPOT_BID_PRICE", "1"),
		AWSAccessKeyID:      os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey:  os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	var missing []string
	if c.Region == "" {
		missing = append(missing, "EC2_REGION")
	}
	if c.AvailabilityZone == "" {
		missing = append(missing, "EC2_AVAILABILITY_ZONE")
	}
	if c.VPCID == "" {
		missing = append(missing, "EC2_VPC_ID")
	}
	if c.SecurityGroup == "" {
		missing = append(missing, "EC2_SECURITY_GROUP")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %v", missing)
	}
	if c.DefaultRequestType != "spot" && c.DefaultRequestType != "ondemand" {
		return nil, fmt.Errorf("EC2_REQUEST_TYPE must be 'spot' or 'ondemand', got %q", c.DefaultRequestType)
	}
	return c, nil
}

func (c *EnvConfig) RequireForLaunch() error {
	var missing []string
	if c.AMIID == "" {
		missing = append(missing, "EC2_AMI_ID")
	}
	if c.PublicKey == "" {
		missing = append(missing, "EC2_PUBLIC_KEY")
	}
	if c.InstanceRole == "" {
		missing = append(missing, "EC2_ROLE")
	}
	if c.AWSAccessKeyID == "" {
		missing = append(missing, "AWS_ACCESS_KEY_ID")
	}
	if c.AWSSecretAccessKey == "" {
		missing = append(missing, "AWS_SECRET_ACCESS_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("launch requires env vars: %v", missing)
	}
	return nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
