package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"ec2cp/src/config"
	"ec2cp/src/ec2"
	"ec2cp/src/progress"
	"ec2cp/src/tasks"

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
		Owner            string `json:"owner,omitempty"`
		AvailabilityZone string `json:"availabilityZone,omitempty"`
		InstanceType     string `json:"instanceType,omitempty"`
		VolumeSize       *int   `json:"volumeSize,omitempty"`
		RequestType      string `json:"requestType,omitempty"`
	}
	out := make([]instanceJSON, 0, len(insts))
	for name, cfg := range insts {
		out = append(out, instanceJSON{
			Name:             name,
			Owner:            cfg.Owner,
			AvailabilityZone: cfg.AvailabilityZone,
			InstanceType:     cfg.InstanceType,
			VolumeSize:       cfg.VolumeSize,
			RequestType:      cfg.RequestType,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, map[string]any{"instances": out})
}

// handleInstanceCreate adds a new instance to instances.json. Body: {"name": "..."}.
// The entry starts empty — defaults come from env/overrides at launch time.
func handleInstanceCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(w, "instance name is required", http.StatusBadRequest)
		return
	}
	if err := config.AddInstance(name, config.InstanceConfig{}); err != nil {
		if errors.Is(err, config.ErrInstanceExists) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name})
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

// instanceTypesCache memoizes per-AZ instance types with their specs.
// The list is large (~700–900 entries) and rarely changes, so a process-lifetime cache is fine.
var instanceTypesCache sync.Map // key: az string → value: []InstanceTypeEntry

// InstanceTypeEntry is one item in the instance-types dropdown — a type name
// plus its hardware specs, so the UI can render specs inline.
type InstanceTypeEntry struct {
	Name      string    `json:"name"`
	VCpus     int32     `json:"vCpus,omitempty"`
	MemoryMiB int64     `json:"memoryMiB,omitempty"`
	Gpus      []GpuInfo `json:"gpus,omitempty"`
}

const instanceTypesBatchSize = 100 // AWS DescribeInstanceTypes hard limit

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

		var typeNames []string
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
				typeNames = append(typeNames, string(o.InstanceType))
			}
		}
		sort.Strings(typeNames)

		entries, err := fetchInstanceTypeSpecs(ctx, client, typeNames)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		instanceTypesCache.Store(az, entries)
		writeJSON(w, map[string]any{"types": entries, "az": az, "cached": false})
	}
}

// fetchInstanceTypeSpecs calls DescribeInstanceTypes in batches of 100
// (AWS limit) concurrently and returns the entries in the same order as names.
// Missing entries (rare) are returned as name-only with zero specs.
func fetchInstanceTypeSpecs(ctx context.Context, client *awsec2.Client, names []string) ([]InstanceTypeEntry, error) {
	specs := make(map[string]InstanceTypeEntry, len(names))
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, (len(names)+instanceTypesBatchSize-1)/instanceTypesBatchSize)

	for i := 0; i < len(names); i += instanceTypesBatchSize {
		end := i + instanceTypesBatchSize
		if end > len(names) {
			end = len(names)
		}
		batch := names[i:end]
		wg.Add(1)
		go func(batch []string) {
			defer wg.Done()
			typeArr := make([]types.InstanceType, len(batch))
			for j, n := range batch {
				typeArr[j] = types.InstanceType(n)
			}
			out, err := client.DescribeInstanceTypes(ctx, &awsec2.DescribeInstanceTypesInput{
				InstanceTypes: typeArr,
			})
			if err != nil {
				errCh <- err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, t := range out.InstanceTypes {
				specs[string(t.InstanceType)] = buildEntry(t)
			}
		}(batch)
	}
	wg.Wait()
	close(errCh)
	if err, ok := <-errCh; ok {
		return nil, err
	}

	out := make([]InstanceTypeEntry, 0, len(names))
	for _, n := range names {
		if e, ok := specs[n]; ok {
			out = append(out, e)
		} else {
			out = append(out, InstanceTypeEntry{Name: n})
		}
	}
	return out, nil
}

func buildEntry(t types.InstanceTypeInfo) InstanceTypeEntry {
	e := InstanceTypeEntry{Name: string(t.InstanceType)}
	if t.VCpuInfo != nil {
		e.VCpus = aws.ToInt32(t.VCpuInfo.DefaultVCpus)
	}
	if t.MemoryInfo != nil {
		e.MemoryMiB = aws.ToInt64(t.MemoryInfo.SizeInMiB)
	}
	if t.GpuInfo != nil {
		for _, g := range t.GpuInfo.Gpus {
			gpu := GpuInfo{Count: aws.ToInt32(g.Count), Name: aws.ToString(g.Name)}
			if g.MemoryInfo != nil {
				gpu.MemoryMiB = aws.ToInt32(g.MemoryInfo.SizeInMiB)
			}
			e.Gpus = append(e.Gpus, gpu)
		}
	}
	return e
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
		Owner:              inst.Owner,
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

