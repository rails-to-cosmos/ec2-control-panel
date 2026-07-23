package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"ec2cp/src/config"
	"ec2cp/src/ec2"
	"ec2cp/src/progress"
	"ec2cp/src/tasks"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ---- discovery / config endpoints ----

// handleInstances lists instances, filtered to those the requesting user may
// read when auth is enabled (admins and unauthenticated mode see all).
func handleInstances(auth *AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		user, isAdmin := auth.reader(r)
		out := make([]instanceJSON, 0, len(insts))
		for name, cfg := range insts {
			if !cfg.CanRead(user, isAdmin) {
				continue
			}
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
}

// handleStatuses returns compact cached status for every instance the user may
// read — one row per instance for the UI table. Reads the cache only (no AWS
// calls); the background poller keeps it fresh.
func handleStatuses(cache *ec2.Cache, auth *AuthConfig) http.HandlerFunc {
	type statusJSON struct {
		Name         string `json:"name"`
		State        string `json:"state"`
		InstanceType string `json:"instanceType,omitempty"`
		IP           string `json:"ip,omitempty"`
		Lifecycle    string `json:"lifecycle,omitempty"`
		VCpus        int32  `json:"vCpus,omitempty"`
		MemoryMiB    int64  `json:"memoryMiB,omitempty"`
		LaunchTime   string `json:"launchTime,omitempty"`
		AsOf         string `json:"asOf,omitempty"`
		Error        string `json:"error,omitempty"`
		Pending      bool   `json:"pending,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		insts, err := config.LoadInstances()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		user, isAdmin := auth.reader(r)
		out := make([]statusJSON, 0, len(insts))
		for name, cfg := range insts {
			if !cfg.CanRead(user, isAdmin) {
				continue
			}
			s := statusJSON{Name: name}
			snap := cache.Get(name)
			if snap == nil {
				s.Pending = true
			} else {
				s.Error = snap.FetchErr
				if !snap.AsOf.IsZero() {
					s.AsOf = snap.AsOf.Format(time.RFC3339)
				}
				if snap.Instance != nil {
					s.State = snap.Instance.State
					s.InstanceType = snap.Instance.InstanceType
					s.IP = snap.Instance.PrivateIP
					s.Lifecycle = snap.Instance.Lifecycle
					s.VCpus = snap.Instance.VCpus
					s.MemoryMiB = snap.Instance.MemoryMiB
					if !snap.Instance.LaunchTime.IsZero() {
						s.LaunchTime = snap.Instance.LaunchTime.Format(time.RFC3339)
					}
				} else {
					s.State = "none"
				}
			}
			out = append(out, s)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, map[string]any{"statuses": out})
	}
}

// handleWhoami reports the auth state and current user for the UI header.
func handleWhoami(auth *AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth == nil {
			writeJSON(w, map[string]any{"authEnabled": false})
			return
		}
		realUser := UserFromContext(r.Context())
		realIsAdmin := auth.isAdmin(realUser)
		// user/isAdmin describe the identity the page is rendered as, which
		// differs from the real one while an admin is impersonating.
		user, isAdmin := auth.reader(r)
		viewingAs := ""
		if user != realUser {
			viewingAs = user
		}
		writeJSON(w, map[string]any{
			"authEnabled": true,
			"user":        user,
			"isAdmin":     isAdmin,
			"realUser":    realUser,
			"realIsAdmin": realIsAdmin,
			"viewingAs":   viewingAs,
			"logoutUrl":   auth.p("/logout"),
		})
	}
}

// handleInstanceCreate adds a new instance to instances.json.
// Body: {"name": "...", "readers": ["user", ...]}.
// An empty readers list means visible to everyone; a non-empty list is stored
// with the creating user always included so they can't lock themselves out.
func handleInstanceCreate(auth *AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var body struct {
			Name    string   `json:"name"`
			Readers []string `json:"readers"`
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
		readers := normalizeReaders(body.Readers)
		if len(readers) > 0 && auth != nil {
			if creator := UserFromContext(r.Context()); creator != "" && !slices.Contains(readers, creator) {
				readers = append(readers, creator)
			}
		}
		if err := config.AddInstance(name, config.InstanceConfig{Readers: readers}); err != nil {
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
}

// normalizeReaders trims, drops blanks, and de-duplicates a readers list.
func normalizeReaders(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range in {
		if r = strings.TrimSpace(r); r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
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
		entries, err := instanceTypesForAZ(r.Context(), env, az)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"types": entries, "az": az})
	}
}

// instanceTypesForAZ returns the offered instance types (with specs) for AZ,
// memoized process-wide. Shared by the HTTP handler and the startup warmer.
func instanceTypesForAZ(ctx context.Context, env *config.EnvConfig, az string) ([]InstanceTypeEntry, error) {
	if cached, ok := instanceTypesCache.Load(az); ok {
		return cached.([]InstanceTypeEntry), nil
	}
	client, err := ec2.NewClient(ctx, env.Region)
	if err != nil {
		return nil, err
	}
	var typeNames []string
	paginator := awsec2.NewDescribeInstanceTypeOfferingsPaginator(client, &awsec2.DescribeInstanceTypeOfferingsInput{
		LocationType: types.LocationTypeAvailabilityZone,
		Filters:      []types.Filter{{Name: aws.String("location"), Values: []string{az}}},
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.InstanceTypeOfferings {
			typeNames = append(typeNames, string(o.InstanceType))
		}
	}
	sort.Strings(typeNames)

	entries, err := fetchInstanceTypeSpecs(ctx, client, typeNames)
	if err != nil {
		return nil, err
	}
	instanceTypesCache.Store(az, entries)
	return entries, nil
}

// azCache memoizes the region's availability zones (they effectively never change).
var azCache sync.Map // key: region → value: []string

// availabilityZones lists the region's usable AZ names, memoized.
func availabilityZones(ctx context.Context, env *config.EnvConfig) ([]string, error) {
	if v, ok := azCache.Load(env.Region); ok {
		return v.([]string), nil
	}
	client, err := ec2.NewClient(ctx, env.Region)
	if err != nil {
		return nil, err
	}
	out, err := client.DescribeAvailabilityZones(ctx, &awsec2.DescribeAvailabilityZonesInput{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.AvailabilityZones))
	for _, z := range out.AvailabilityZones {
		if z.State == types.AvailabilityZoneStateAvailable {
			names = append(names, aws.ToString(z.ZoneName))
		}
	}
	sort.Strings(names)
	azCache.Store(env.Region, names)
	return names, nil
}

func handleAZs(env *config.EnvConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		names, err := availabilityZones(r.Context(), env)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"azs": names})
	}
}

// priceCache memoizes the price pair per "type|az". Prices drift, but the
// column is explicitly approximate, so process-lifetime caching is fine.
var priceCache sync.Map // key: "type|az" → value: map[string]any

// pricesFor returns the approximate hourly spot and on-demand prices for TYPE.
// Spot is per-AZ and current; on-demand is per-region and fixed. A failure on
// either side leaves that figure empty rather than failing the whole lookup.
func pricesFor(ctx context.Context, env *config.EnvConfig, instType, az string) (map[string]any, error) {
	key := instType + "|" + az
	if v, ok := priceCache.Load(key); ok {
		return v.(map[string]any), nil
	}
	client, err := ec2.NewClient(ctx, env.Region)
	if err != nil {
		return nil, err
	}
	spot := ""
	out, err := client.DescribeSpotPriceHistory(ctx, &awsec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []types.InstanceType{types.InstanceType(instType)},
		ProductDescriptions: []string{"Linux/UNIX"},
		AvailabilityZone:    aws.String(az),
		StartTime:           aws.Time(time.Now()),
	})
	if err == nil {
		var latest time.Time
		for _, p := range out.SpotPriceHistory {
			if ts := aws.ToTime(p.Timestamp); aws.ToString(p.SpotPrice) != "" && ts.After(latest) {
				latest, spot = ts, aws.ToString(p.SpotPrice)
			}
		}
	}
	onDemand, odErr := ec2.OnDemandPrice(ctx, env.Region, instType)
	if err != nil && odErr != nil {
		return nil, err // both lookups failed
	}
	resp := map[string]any{"type": instType, "az": az, "spot": spot, "onDemand": onDemand}
	priceCache.Store(key, resp)
	return resp, nil
}

// handlePrice serves the approximate hourly spot + on-demand price for a type + AZ.
func handlePrice(env *config.EnvConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		instType := r.URL.Query().Get("type")
		if instType == "" {
			http.Error(w, "type query param required", http.StatusBadRequest)
			return
		}
		az := r.URL.Query().Get("az")
		if az == "" {
			az = env.AvailabilityZone
		}
		resp, err := pricesFor(r.Context(), env, instType, az)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, resp)
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

type GpuInfo struct {
	Count     int32  `json:"count"`
	Name      string `json:"name"`
	MemoryMiB int32  `json:"memoryMiB,omitempty"`
}

// ---- async submitters (task queue) ----

func resolveAZForRequest(env *config.EnvConfig, r *http.Request, sessionID string) (string, *config.InstanceConfig, error) {
	inst, err := config.GetInstance(sessionID)
	if err != nil {
		return "", nil, err
	}
	az := ec2.FirstNonEmpty(r.URL.Query().Get("az"), inst.AvailabilityZone, env.AvailabilityZone)
	return az, inst, nil
}

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
