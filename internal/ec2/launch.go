// Package ec2 contains the business logic for the per-user EC2 sandbox: the
// status / start / stop / restart / ip / mount operations the CLI and HTTP
// server both invoke. AWS SDK dependencies are aliased as `awsec2` to keep
// `ec2` as the package name for our domain.
package ec2

import "ec2cp/internal/config"

// LaunchParams is the resolved input to Start (and the start phase of Restart).
// Source labels are populated alongside each value so the start report can
// show "where this came from" (CLI flag vs instances.json vs env).
//
// SessionID is the cache key / dropdown label. AWSName is the value used for
// the Name tag and AWS lookups (= SessionID unless instances.json overrides).
type LaunchParams struct {
	SessionID    string
	AWSName      string
	InstanceName string
	InstanceType string
	RequestType  string
	VolumeSize   int // root volume size for the actual instance
	Env          *config.EnvConfig
	AZ           string
	BidPrice     string

	InstanceNameSource string
	InstanceTypeSource string
	RequestTypeSource  string
	AZSource           string
	BidPriceSource     string
}

// ResolveSource returns the first non-empty of (flag, override, def) along
// with a label describing where it came from.
func ResolveSource(flag, override, def, flagName, overrideName, defName string) (value, source string) {
	switch {
	case flag != "":
		return flag, "--" + flagName
	case override != "":
		return override, "instances.json:" + overrideName
	default:
		return def, "env:" + defName
	}
}
