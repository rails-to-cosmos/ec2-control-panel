package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

//go:embed ui/index.html
var uiFS embed.FS

func serveCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP server (replaces the marimo UI)",
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := loadEnvConfig()
			if err != nil {
				return err
			}
			return runServer(cmd.Context(), env, port)
		},
	}
	cmd.Flags().IntVar(&port, "port", 2721, "listen port")
	return cmd
}

func runServer(ctx context.Context, env *EnvConfig, port int) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		page, err := uiFS.ReadFile("ui/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /api/instances", handleInstances)
	mux.HandleFunc("GET /api/status/{id}", withStream(env, runStatusOp))
	mux.HandleFunc("POST /api/start/{id}", withStream(env, runStartOp))
	mux.HandleFunc("POST /api/stop/{id}", withStream(env, runStopOp))
	mux.HandleFunc("POST /api/restart/{id}", withStream(env, runRestartOp))
	mux.HandleFunc("GET /api/ip/{id}", withStream(env, runIPOp))
	mux.HandleFunc("POST /api/mount/{volume}/{id}", withStream(env, runMountOp))

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("ec2cp serve listening on %s\n", addr)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func handleInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := loadInstances()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := make([]string, 0, len(insts))
	for name := range insts {
		names = append(names, name)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"instances": names})
}

// progressWriter flushes on every write so streamed lines reach the client immediately.
type progressWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.flusher.Flush()
	return n, err
}

// withStream wraps an op into an HTTP handler that streams logf output back to the client
// as plain chunked text. Op-specific arg parsing (sessionID, query flags) lives in each op.
func withStream(env *EnvConfig, op func(ctx context.Context, env *EnvConfig, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported by this transport", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if proxied
		flusher.Flush()

		ctx := contextWithLogger(r.Context(), &progressWriter{w: w, flusher: flusher})
		if err := op(ctx, env, r); err != nil {
			fmt.Fprintf(&progressWriter{w: w, flusher: flusher}, "\nERROR: %v\n", err)
		}
	}
}

// ---- per-op handlers ----

func resolveAZForRequest(env *EnvConfig, r *http.Request, sessionID string) (string, *InstanceConfig, error) {
	inst, err := getInstanceConfig(sessionID)
	if err != nil {
		return "", nil, err
	}
	az := firstNonEmpty(r.URL.Query().Get("az"), inst.AvailabilityZone, env.AvailabilityZone)
	return az, inst, nil
}

func runStatusOp(ctx context.Context, env *EnvConfig, r *http.Request) error {
	sessionID := r.PathValue("id")
	az, _, err := resolveAZForRequest(env, r, sessionID)
	if err != nil {
		return err
	}
	return runStatus(ctx, env, sessionID, az)
}

func runIPOp(ctx context.Context, env *EnvConfig, r *http.Request) error {
	sessionID := r.PathValue("id")
	az, _, err := resolveAZForRequest(env, r, sessionID)
	if err != nil {
		return err
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(env.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	client := ec2.NewFromConfig(awsCfg)
	_, instanceID, err := getVolume(ctx, client, sessionID, az)
	if err != nil {
		return err
	}
	if instanceID == "" {
		return fmt.Errorf("no running instance for %q", sessionID)
	}
	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return err
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return fmt.Errorf("instance %s vanished", instanceID)
	}
	logf(ctx, "%s\n", *out.Reservations[0].Instances[0].PrivateIpAddress)
	return nil
}

func runStopOp(ctx context.Context, env *EnvConfig, r *http.Request) error {
	sessionID := r.PathValue("id")
	az, _, err := resolveAZForRequest(env, r, sessionID)
	if err != nil {
		return err
	}
	force := r.URL.Query().Get("force") == "true"
	// HTTP callers always bypass the interactive y/N prompt — the UI handles confirmation.
	return runStop(ctx, env, sessionID, az, force, true)
}

func runStartOp(ctx context.Context, env *EnvConfig, r *http.Request) error {
	sessionID := r.PathValue("id")
	az, inst, err := resolveAZForRequest(env, r, sessionID)
	if err != nil {
		return err
	}
	if err := env.requireForLaunch(); err != nil {
		return err
	}
	q := r.URL.Query()
	rType, rTypeSrc := resolveSource(q.Get("requestType"), inst.RequestType, env.DefaultRequestType,
		"requestType (query)", "request_type", "EC2_REQUEST_TYPE")
	if rType != "spot" && rType != "ondemand" {
		return fmt.Errorf("invalid request type %q", rType)
	}
	iType, iTypeSrc := resolveSource(q.Get("instanceType"), inst.InstanceType, env.DefaultInstanceType,
		"instanceType (query)", "instance_type", "EC2_INSTANCE_TYPE")
	bidPrice, bidPriceSrc := resolveSource(q.Get("bidPrice"), "", env.BidPrice,
		"bidPrice (query)", "", "EC2_SPOT_BID_PRICE")
	_, azSrc := resolveSource(q.Get("az"), inst.AvailabilityZone, env.AvailabilityZone,
		"az (query)", "availability_zone", "EC2_AVAILABILITY_ZONE")

	name, nameSrc := q.Get("instanceName"), "instanceName (query)"
	if name == "" {
		name, nameSrc = sessionID, "session-id default"
	}

	return runStart(ctx, LaunchParams{
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
	})
}

func runRestartOp(ctx context.Context, env *EnvConfig, r *http.Request) error {
	if err := runStopOp(ctx, env, r); err != nil {
		return fmt.Errorf("stop phase: %w", err)
	}
	return runStartOp(ctx, env, r)
}

func runMountOp(ctx context.Context, env *EnvConfig, r *http.Request) error {
	// For now: mount-without-create. EFS auto-create from HTTP requires a UX decision
	// (the CLI prompts y/N; the API would need a ?create=true flag). Defer.
	return fmt.Errorf("mount over HTTP not yet supported (use the CLI)")
}
