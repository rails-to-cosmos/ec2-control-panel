package ec2

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ec2cp/internal/config"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
)

// Snapshot is the structured view of a session's EC2 sandbox state at a point
// in time. SessionID is the cache key (= the instances.json key); AWSName is
// the Name tag used to look up AWS resources. They differ when the entry
// declares a `name` override.
type Snapshot struct {
	SessionID string
	AWSName   string
	AsOf      time.Time
	Region    string
	AZ        string
	VPC       string
	Subnet    string
	Volume    string
	ENI       string
	Instance  *InstanceDetails // nil when no instance is attached
	FetchErr  string           // populated when fetching failed; partial fields may still be set
}

// Fetch performs the read-only AWS calls needed to populate a Snapshot.
// It never returns nil — on error, the snapshot's FetchErr is populated
// instead so callers can still display whatever partial info was gathered.
//
// Subnet, Volume and ENI lookups are independent and run in parallel; the
// instance-details fetch chains on the Volume's attached-instance-id.
//
// awsName is what's queried for the Name tag (may differ from sessionID when
// a `name` override is set in instances.json).
func Fetch(ctx context.Context, client *awsec2.Client, env *config.EnvConfig, sessionID, awsName, az string) *Snapshot {
	snap := &Snapshot{
		SessionID: sessionID,
		AWSName:   awsName,
		AsOf:      time.Now(),
		Region:    env.Region,
		AZ:        az,
		VPC:       env.VPCID,
	}

	var (
		subnetID, volumeID, attachedInstanceID, eniID string
		subnetErr, volumeErr, eniErr                  error
		wg                                            sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		subnetID, subnetErr = GetSubnetID(ctx, client, env.VPCID, az)
	}()
	go func() {
		defer wg.Done()
		volumeID, attachedInstanceID, volumeErr = GetVolume(ctx, client, awsName, az)
	}()
	go func() {
		defer wg.Done()
		eniID, eniErr = GetENIID(ctx, client, awsName, az)
	}()
	wg.Wait()

	snap.Subnet = subnetID
	snap.Volume = volumeID
	snap.ENI = eniID

	switch {
	case subnetErr != nil:
		snap.FetchErr = fmt.Sprintf("subnet lookup: %v", subnetErr)
		return snap
	case volumeErr != nil:
		snap.FetchErr = fmt.Sprintf("volume lookup: %v", volumeErr)
		return snap
	case eniErr != nil:
		// ENI is best-effort — record but don't bail.
		snap.FetchErr = fmt.Sprintf("eni lookup: %v", eniErr)
	}

	if attachedInstanceID != "" {
		details, err := describeInstance(ctx, client, attachedInstanceID)
		if err != nil {
			snap.FetchErr = fmt.Sprintf("describe instance: %v", err)
			return snap
		}
		snap.Instance = details
	}

	return snap
}
