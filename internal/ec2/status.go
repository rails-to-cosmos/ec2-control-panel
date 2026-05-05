package ec2

import (
	"context"
	"time"

	"ec2cp/internal/config"
	"ec2cp/internal/progress"
)

const notFound = "Not found"

// Status (CLI path) fetches a snapshot synchronously and renders it. The HTTP
// server uses RenderText against a cached snapshot.
func Status(ctx context.Context, env *config.EnvConfig, sessionID, awsName, az string) error {
	client, err := NewClient(ctx, env.Region)
	if err != nil {
		return err
	}
	RenderText(ctx, Fetch(ctx, client, env, sessionID, awsName, az))
	return nil
}

// RenderText writes the standard CLI-style status output for snap to the
// context-bound logger.
func RenderText(ctx context.Context, snap *Snapshot) {
	progress.Logf(ctx, "Session ID: %s\n", snap.SessionID)
	if snap.AWSName != "" && snap.AWSName != snap.SessionID {
		progress.Logf(ctx, "AWS name:   %s\n", snap.AWSName)
	}
	progress.Logf(ctx, "VPC: %s\n", snap.VPC)
	progress.Logf(ctx, "Region: %s\n", snap.Region)
	progress.Logf(ctx, "Availability zone: %s\n", snap.AZ)

	if snap.Instance != nil {
		printInstance(ctx, snap.Instance)
	} else {
		progress.Logf(ctx, "Instance: %s\n", notFound)
	}

	progress.Logf(ctx, "Subnet: %s\n", orNotFound(snap.Subnet))
	progress.Logf(ctx, "Volume: %s\n", orNotFound(snap.Volume))
	progress.Logf(ctx, "Network: %s\n", orNotFound(snap.ENI))

	if snap.FetchErr != "" {
		progress.Logf(ctx, "\n(fetch error: %s)\n", snap.FetchErr)
	}
	if !snap.AsOf.IsZero() {
		age := time.Since(snap.AsOf).Round(time.Second)
		progress.Logf(ctx, "\n(as of %s, %s ago)\n", snap.AsOf.Format(time.RFC3339), age)
	}
}

func orNotFound(s string) string {
	if s == "" {
		return notFound
	}
	return s
}
