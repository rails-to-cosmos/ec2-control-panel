package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

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
	path, err := findInstancesFile()
	if err != nil {
		path, err = createDefaultInstancesFile()
		if err != nil {
			return nil, err
		}
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
