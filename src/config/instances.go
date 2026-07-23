package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// ErrInstanceExists is returned by AddInstance when the session id is already
// present in instances.json.
var ErrInstanceExists = errors.New("instance already exists")

// instancesMu serializes the read-modify-write in AddInstance so concurrent
// requests can't lose entries. In-process only — a CLI run or a manual edit
// racing the server still resolves last-writer-wins.
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
	// Readers lists the usernames allowed to see and control this instance.
	// Empty means admins only; ReadersPublic ("*") means any authenticated
	// user. Admins always have access.
	Readers []string `json:"readers,omitempty"`
}

// ReadersPublic is the Readers entry that opens an instance to every
// authenticated user. Visibility is closed by default, so "public" has to be
// stated explicitly rather than implied by an empty list.
const ReadersPublic = "*"

// CanRead reports whether USER may see and control this instance. ISADMIN
// grants access unconditionally. Otherwise Readers decides: ReadersPublic
// means any authenticated user, and an empty list means admins only.
func (c *InstanceConfig) CanRead(user string, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	if slices.Contains(c.Readers, ReadersPublic) {
		return true
	}
	return slices.Contains(c.Readers, user)
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

// UpdateInstance applies APPLY to an existing entry and persists the file.
// Taking a callback keeps every field the caller doesn't touch intact, and
// reuses the same lock + write path as AddInstance.
func UpdateInstance(sessionID string, apply func(*InstanceConfig)) error {
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
	cfg, ok := insts[sessionID]
	if !ok {
		return fmt.Errorf("unknown instance %q", sessionID)
	}
	apply(&cfg)
	insts[sessionID] = cfg
	return writeInstances(path, insts)
}

// writeInstances marshals INSTS (map keys sorted by encoding/json) and writes
// it to PATH, preferring an atomic replace so readers never see a half-written
// file.
//
// The atomic path can't always be used: in production instances.json is a
// single-file bind mount, and renaming onto a mount point fails with EBUSY. In
// that case we fall back to writing in place, which is what the mount requires.
func writeInstances(path string, insts Instances) error {
	data, err := json.MarshalIndent(insts, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(path, data)
}

// WriteFileAtomic writes DATA to PATH, preferring a temp-file + rename so a
// partial write can never truncate the existing file, and falling back to an
// in-place write when the rename can't work — in production instances.json is a
// single-file bind mount and renaming onto a mount point fails with EBUSY.
// Every JSON store in the app goes through this.
func WriteFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(path, data); err == nil {
		return nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instances-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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
	if p, ok := findUpwards("instances.json"); ok {
		return p, nil
	}
	return "", fmt.Errorf("instances.json not found in cwd or any ancestor")
}

// findUpwards looks for NAME in the cwd and every ancestor directory.
func findUpwards(name string) (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
