package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrInstanceExists is returned by AddInstance when the session id is already
// present in instances.json.
var ErrInstanceExists = errors.New("instance already exists")

// instancesMu serializes the read-modify-write in AddInstance so concurrent
// requests can't lose entries. Combined with the atomic rename in
// writeInstances, readers always see a complete file.
var instancesMu sync.Mutex

type InstanceConfig struct {
	// Name overrides the AWS-resource Name tag. The JSON key remains the
	// dropdown label / cache key, so two entries with name="ivan" but
	// different keys / AZs become two separate logical sessions sharing
	// the same AWS Name tag in different AZs.
	Name             string `json:"name,omitempty"`
	Owner            string `json:"owner,omitempty"`
	AvailabilityZone string `json:"availability_zone,omitempty"`
	InstanceType     string `json:"instance_type,omitempty"`
	VolumeSize       *int   `json:"volume_size,omitempty"`
	RequestType      string `json:"request_type,omitempty"`
}

// AWSName returns the Name tag to use for this entry's AWS resources,
// falling back to sessionID (the JSON key) when no override is set.
func (c *InstanceConfig) AWSName(sessionID string) string {
	if c.Name != "" {
		return c.Name
	}
	return sessionID
}

type Instances map[string]InstanceConfig

func LoadInstances() (Instances, error) {
	path, err := resolveInstancesPath()
	if err != nil {
		return nil, err
	}
	return loadInstancesFrom(path)
}

// loadInstancesFrom decodes the instances file at PATH.
func loadInstancesFrom(path string) (Instances, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var insts Instances
	if err := dec.Decode(&insts); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return insts, nil
}

// AddInstance adds a new entry keyed by SESSIONID and persists instances.json.
// Returns ErrInstanceExists if the id is already present.
func AddInstance(sessionID string, cfg InstanceConfig) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("instance name is required")
	}
	instancesMu.Lock()
	defer instancesMu.Unlock()
	path, err := resolveInstancesPath()
	if err != nil {
		return err
	}
	insts, err := loadInstancesFrom(path)
	if err != nil {
		return err
	}
	if _, ok := insts[sessionID]; ok {
		return fmt.Errorf("%q: %w", sessionID, ErrInstanceExists)
	}
	insts[sessionID] = cfg
	return writeInstances(path, insts)
}

// writeInstances marshals INSTS (map keys sorted by encoding/json) and writes
// it to PATH atomically via a temp file + rename, so a failed or partial write
// can't truncate the existing file and readers never see a half-written one.
func writeInstances(path string, insts Instances) error {
	data, err := json.MarshalIndent(insts, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".instances-*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// resolveInstancesPath returns the instances.json path, creating an empty file
// in the cwd when none exists in the cwd or any ancestor.
func resolveInstancesPath() (string, error) {
	path, err := findInstancesFile()
	if err != nil {
		return createDefaultInstancesFile()
	}
	return path, nil
}

func GetInstance(sessionID string) (*InstanceConfig, error) {
	insts, err := LoadInstances()
	if err != nil {
		return nil, err
	}
	cfg, ok := insts[sessionID]
	if !ok {
		return nil, fmt.Errorf("unknown instance %q. Add it to instances.json", sessionID)
	}
	return &cfg, nil
}

func createDefaultInstancesFile() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "instances.json")
	if err := os.WriteFile(p, []byte("{}\n"), 0644); err != nil {
		return "", fmt.Errorf("creating default instances.json: %w", err)
	}
	return p, nil
}

func findInstancesFile() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, "instances.json")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("instances.json not found in cwd or any ancestor")
		}
		dir = parent
	}
}
