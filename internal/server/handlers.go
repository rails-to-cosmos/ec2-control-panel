package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"

	"ec2cp/internal/config"
	"ec2cp/internal/ec2"
	"ec2cp/internal/progress"
	"ec2cp/internal/tasks"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ---- discovery / config endpoints ----

func handleInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := config.LoadInstances()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type instanceJSON struct {
		Name             string `json:"name"`
		AvailabilityZone string `json:"availabilityZone,omitempty"`
		InstanceType     string `json:"instanceType,omitempty"`
		VolumeSize       *int   `json:"volumeSize,omitempty"`
		RequestType      string `json:"requestType,omitempty"`
	}
	out := make([]instanceJSON, 0, len(insts))
	for name, cfg := range insts {
		out = append(out, instanceJSON{
			Name:             name,
			AvailabilityZone: cfg.AvailabilityZone,
			InstanceType:     cfg.InstanceType,
			VolumeSize:       cfg.VolumeSize,
			RequestType:      cfg.RequestType,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, map[string]any{"instances": out})
}

func handleConfig(env *config.EnvConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"region":              env.Region,
			"availabilityZone":    env.AvailabilityZone,
			"vpcId":               env.VPCID,
			"defaultRequestType":  env.DefaultRequestType,
			"defaultInstanceType": env.DefaultInstanceType,
			"defaultBidPrice":     env.BidPrice,
		})
	}
}

// instanceTypesCache memoizes per-AZ DescribeInstanceTypeOfferings.
// The list is large (~700–900 entries) and rarely changes, so a process-lifetime cache is fine.
var instanceTypesCache sync.Map // key: az string → value: []string

func handleInstanceTypes(env *config.EnvConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		az := r.URL.Query().Get("az")
		if az == "" {
			az = env.AvailabilityZone
		}
		if cached, ok := instanceTypesCache.Load(az); ok {
			writeJSON(w, map[string]any{"types": cached, "az": az, "cached": true})
			return
		}

		ctx := r.Context()
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		client := awsec2.NewFromConfig(awsCfg)

		var typeList []string
		paginator := awsec2.NewDescribeInstanceTypeOfferingsPaginator(client, &awsec2.DescribeInstanceTypeOfferingsInput{
			LocationType: types.LocationTypeAvailabilityZone,
			Filters: []types.Filter{
				{Name: aws.String("location"), Values: []string{az}},
			},
		})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, o := range page.InstanceTypeOfferings {
				typeList = append(typeList, string(o.InstanceType))
			}
		}
		sort.Strings(typeList)
		instanceTypesCache.Store(az, typeList)
		writeJSON(w, map[string]any{"types": typeList, "az": az, "cached": false})
	}
}

// ---- per-op handlers (synchronous, streamed) ----

func resolveAZForRequest(env *config.EnvConfig, r *http.Request, sessionID string) (string, *config.InstanceConfig, error) {
	inst, err := config.GetInstance(sessionID)
	if err != nil {
		return "", nil, err
	}
	az := firstNonEmpty(r.URL.Query().Get("az"), inst.AvailabilityZone, env.AvailabilityZone)
	return az, inst, nil
}

func runStatusOp(ctx context.Context, env *config.EnvConfig, r *http.Request) error {
	sessionID := r.PathValue("id")
	az, _, err := resolveAZForRequest(env, r, sessionID)
	if err != nil {
		return err
	}
	return ec2.Status(ctx, env, sessionID, az)
}

func runIPOp(ctx context.Context, env *config.EnvConfig, r *http.Request) error {
	sessionID := r.PathValue("id")
	az, _, err := resolveAZForRequest(env, r, sessionID)
	if err != nil {
		return err
	}
	return ec2.IP(ctx, env, sessionID, az)
}

func runMountOp(ctx context.Context, env *config.EnvConfig, r *http.Request) error {
	sessionID := r.PathValue("id")
	volumeName := r.PathValue("volume")
	az, _, err := resolveAZForRequest(env, r, sessionID)
	if err != nil {
		return err
	}
	// Pass nil confirmer — UI handles confirmation; auto-create disabled
	// over HTTP for now (CLI-only).
	return ec2.Mount(ctx, env, sessionID, volumeName, az, true, nil)
}

// ---- async submitters (task queue) ----

func handleStartSubmit(env *config.EnvConfig, tm *tasks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := env.RequireForLaunch(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		params, err := buildLaunchParams(env, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		task, err := tm.Create("start", params.SessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		tm.Run(task, func(ctx context.Context, out io.Writer) error {
			return ec2.Start(progress.WithLogger(ctx, out), params)
		})
		writeJSON(w, map[string]any{"taskId": task.ID})
	}
}

func handleStopSubmit(env *config.EnvConfig, tm *tasks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		az, _, err := resolveAZForRequest(env, r, sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		force := r.URL.Query().Get("force") == "true"
		task, err := tm.Create("stop", sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		tm.Run(task, func(ctx context.Context, out io.Writer) error {
			return ec2.Stop(progress.WithLogger(ctx, out), env, sessionID, az, force, true, nil)
		})
		writeJSON(w, map[string]any{"taskId": task.ID})
	}
}

func handleRestartSubmit(env *config.EnvConfig, tm *tasks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := env.RequireForLaunch(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		params, err := buildLaunchParams(env, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		task, err := tm.Create("restart", params.SessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		tm.Run(task, func(ctx context.Context, out io.Writer) error {
			lctx := progress.WithLogger(ctx, out)
			if err := ec2.Stop(lctx, env, params.SessionID, params.AZ, false, true, nil); err != nil {
				return fmt.Errorf("stop phase: %w", err)
			}
			return ec2.Start(lctx, params)
		})
		writeJSON(w, map[string]any{"taskId": task.ID})
	}
}

// buildLaunchParams parses the request once at submit time so the task
// goroutine doesn't depend on the request's lifetime.
func buildLaunchParams(env *config.EnvConfig, r *http.Request) (ec2.LaunchParams, error) {
	sessionID := r.PathValue("id")
	inst, err := config.GetInstance(sessionID)
	if err != nil {
		return ec2.LaunchParams{}, err
	}
	q := r.URL.Query()
	rType, rTypeSrc := ec2.ResolveSource(q.Get("requestType"), inst.RequestType, env.DefaultRequestType,
		"requestType (query)", "request_type", "EC2_REQUEST_TYPE")
	if rType != "spot" && rType != "ondemand" {
		return ec2.LaunchParams{}, fmt.Errorf("invalid request type %q", rType)
	}
	iType, iTypeSrc := ec2.ResolveSource(q.Get("instanceType"), inst.InstanceType, env.DefaultInstanceType,
		"instanceType (query)", "instance_type", "EC2_INSTANCE_TYPE")
	bidPrice, bidPriceSrc := ec2.ResolveSource(q.Get("bidPrice"), "", env.BidPrice,
		"bidPrice (query)", "", "EC2_SPOT_BID_PRICE")
	az, azSrc := ec2.ResolveSource(q.Get("az"), inst.AvailabilityZone, env.AvailabilityZone,
		"az (query)", "availability_zone", "EC2_AVAILABILITY_ZONE")

	name, nameSrc := q.Get("instanceName"), "instanceName (query)"
	if name == "" {
		name, nameSrc = sessionID, "session-id default"
	}

	return ec2.LaunchParams{
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
	}, nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
