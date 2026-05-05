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
		client, err := ec2.NewClient(ctx, env.Region)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

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

// InstanceTypeInfo describes the hardware spec for one EC2 instance type.
type InstanceTypeInfo struct {
	Type      string    `json:"type"`
	VCpus     int32     `json:"vCpus"`
	MemoryMiB int64     `json:"memoryMiB"`
	Gpus      []GpuInfo `json:"gpus,omitempty"`
}

type GpuInfo struct {
	Count     int32  `json:"count"`
	Name      string `json:"name"`
	MemoryMiB int32  `json:"memoryMiB,omitempty"`
}

// instanceInfoCache memoizes per-instance-type DescribeInstanceTypes results.
// Hardware specs never change, so process-lifetime caching is fine.
var instanceInfoCache sync.Map // key: type string → value: InstanceTypeInfo

func handleInstanceInfo(env *config.EnvConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		typeName := r.URL.Query().Get("type")
		if typeName == "" {
			http.Error(w, "type query param required", http.StatusBadRequest)
			return
		}
		if cached, ok := instanceInfoCache.Load(typeName); ok {
			writeJSON(w, cached)
			return
		}
		ctx := r.Context()
		client, err := ec2.NewClient(ctx, env.Region)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out, err := client.DescribeInstanceTypes(ctx, &awsec2.DescribeInstanceTypesInput{
			InstanceTypes: []types.InstanceType{types.InstanceType(typeName)},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(out.InstanceTypes) == 0 {
			http.NotFound(w, r)
			return
		}
		i := out.InstanceTypes[0]
		info := InstanceTypeInfo{Type: typeName}
		if i.VCpuInfo != nil {
			info.VCpus = aws.ToInt32(i.VCpuInfo.DefaultVCpus)
		}
		if i.MemoryInfo != nil {
			info.MemoryMiB = aws.ToInt64(i.MemoryInfo.SizeInMiB)
		}
		if i.GpuInfo != nil {
			for _, g := range i.GpuInfo.Gpus {
				gpu := GpuInfo{
					Count: aws.ToInt32(g.Count),
					Name:  aws.ToString(g.Name),
				}
				if g.MemoryInfo != nil {
					gpu.MemoryMiB = aws.ToInt32(g.MemoryInfo.SizeInMiB)
				}
				info.Gpus = append(info.Gpus, gpu)
			}
		}
		instanceInfoCache.Store(typeName, info)
		writeJSON(w, info)
	}
}

// ---- per-op handlers (synchronous, streamed) ----

func resolveAZForRequest(env *config.EnvConfig, r *http.Request, sessionID string) (string, *config.InstanceConfig, error) {
	inst, err := config.GetInstance(sessionID)
	if err != nil {
		return "", nil, err
	}
	az := ec2.FirstNonEmpty(r.URL.Query().Get("az"), inst.AvailabilityZone, env.AvailabilityZone)
	return az, inst, nil
}

// runStatusOp serves status from the cache; ?force=true (or a cache miss)
// triggers a synchronous Refresh.
func runStatusOp(cache *ec2.Cache) func(ctx context.Context, env *config.EnvConfig, r *http.Request) error {
	return func(ctx context.Context, env *config.EnvConfig, r *http.Request) error {
		sessionID := r.PathValue("id")
		snap := cache.Get(sessionID)
		if snap == nil || r.URL.Query().Get("force") == "true" {
			snap = cache.Refresh(ctx, sessionID)
		}
		ec2.RenderText(ctx, snap)
		return nil
	}
}

func runIPOp(ctx context.Context, env *config.EnvConfig, r *http.Request) error {
	sessionID := r.PathValue("id")
	az, inst, err := resolveAZForRequest(env, r, sessionID)
	if err != nil {
		return err
	}
	return ec2.IP(ctx, env, sessionID, inst.AWSName(sessionID), az)
}

func runMountOp(ctx context.Context, env *config.EnvConfig, r *http.Request) error {
	sessionID := r.PathValue("id")
	volumeName := r.PathValue("volume")
	az, inst, err := resolveAZForRequest(env, r, sessionID)
	if err != nil {
		return err
	}
	// Pass nil confirmer — UI handles confirmation; auto-create disabled
	// over HTTP for now (CLI-only).
	return ec2.Mount(ctx, env, sessionID, inst.AWSName(sessionID), volumeName, az, true, nil)
}

// ---- async submitters (task queue) ----

func handleStartSubmit(env *config.EnvConfig, tm *tasks.Manager, cache *ec2.Cache) http.HandlerFunc {
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
			defer cache.Refresh(ctx, params.SessionID)
			return ec2.Start(progress.WithLogger(ctx, out), params)
		})
		writeJSON(w, map[string]any{"taskId": task.ID})
	}
}

func handleStopSubmit(env *config.EnvConfig, tm *tasks.Manager, cache *ec2.Cache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		az, inst, err := resolveAZForRequest(env, r, sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		awsName := inst.AWSName(sessionID)
		force := r.URL.Query().Get("force") == "true"
		task, err := tm.Create("stop", sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		tm.Run(task, func(ctx context.Context, out io.Writer) error {
			defer cache.Refresh(ctx, sessionID)
			return ec2.Stop(progress.WithLogger(ctx, out), env, sessionID, awsName, az, force, true, nil)
		})
		writeJSON(w, map[string]any{"taskId": task.ID})
	}
}

func handleRestartSubmit(env *config.EnvConfig, tm *tasks.Manager, cache *ec2.Cache) http.HandlerFunc {
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
			defer cache.Refresh(ctx, params.SessionID)
			if err := ec2.Stop(lctx, env, params.SessionID, params.AWSName, params.AZ, false, true, nil); err != nil {
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

	awsName := inst.AWSName(sessionID)
	name, nameSrc := q.Get("instanceName"), "instanceName (query)"
	if name == "" {
		name, nameSrc = awsName, "default"
	}

	return ec2.LaunchParams{
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
	}, nil
}

