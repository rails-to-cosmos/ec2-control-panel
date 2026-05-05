package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type InstanceConfig struct {
	AvailabilityZone string `json:"availability_zone,omitempty"`
	InstanceType     string `json:"instance_type,omitempty"`
	VolumeSize       *int   `json:"volume_size,omitempty"`
	RequestType      string `json:"request_type,omitempty"`
}

type Instances map[string]InstanceConfig

func LoadInstances() (Instances, error) {
	path, err := findInstancesFile()
	if err != nil {
		return nil, err
	}
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
