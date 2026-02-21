package vast

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// InstanceState tracks the lifecycle of a discovered instance.
type InstanceState int

const (
	StateDiscovered InstanceState = iota
	StateConnecting
	StateHealthy
	StateUnhealthy
	StateRemoving
)

func (s InstanceState) String() string {
	switch s {
	case StateDiscovered:
		return "DISCOVERED"
	case StateConnecting:
		return "CONNECTING"
	case StateHealthy:
		return "HEALTHY"
	case StateUnhealthy:
		return "UNHEALTHY"
	case StateRemoving:
		return "REMOVING"
	default:
		return "UNKNOWN"
	}
}

// Instance represents a vast.ai instance from the API.
type Instance struct {
	ID              int                      `json:"id"`
	ActualStatus    string                   `json:"actual_status"`
	PublicIPAddr    string                   `json:"public_ipaddr"`
	SSHHost         string                   `json:"ssh_host"`
	SSHPort         int                      `json:"ssh_port"`
	Ports           map[string][]PortMapping `json:"ports"`
	GPUName         string                   `json:"gpu_name"`
	NumGPUs         int                      `json:"num_gpus"`
	GPUUtil         *float64                 `json:"gpu_util"`
	GPUTemp         *float64                 `json:"gpu_temp"`
	Label           string                   `json:"label"`
	TemplateHashID  string                   `json:"template_hash_id"`
	ExtraEnv        json.RawMessage          `json:"extra_env"`
	Onstart         string                   `json:"onstart"`
	DirectPortStart *int                     `json:"direct_port_start"`
	JupyterToken    string                   `json:"jupyter_token"`

	// Computed fields (not from JSON).
	State          InstanceState `json:"-"`
	StateChangedAt time.Time     `json:"-"`
	ContainerPort  int           `json:"-"`
	DirectSSHPort  int           `json:"-"`
	ModelName      string        `json:"-"`
}

// PortMapping is a single host port mapping entry from the vast.ai API.
type PortMapping struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// InstancesResponse is the top-level API response from GET /instances/.
type InstancesResponse struct {
	Instances []Instance `json:"instances"`
}

var portRe = regexp.MustCompile(`--port\s+(\d+)`)

// ResolveContainerPort determines the SGLang container port from instance config.
func (inst *Instance) ResolveContainerPort() int {
	// Check extra_env for SGLANG_ARGS --port
	env := inst.ParseExtraEnv()
	if args, ok := env["SGLANG_ARGS"]; ok {
		if m := portRe.FindStringSubmatch(args); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil {
				return p
			}
		}
	}
	// Check onstart script
	if m := portRe.FindStringSubmatch(inst.Onstart); m != nil {
		if p, err := strconv.Atoi(m[1]); err == nil {
			return p
		}
	}
	return 8000
}

// ResolveHostPort resolves the host port that maps to the SGLang container port.
func (inst *Instance) ResolveHostPort() int {
	containerPort := inst.ResolveContainerPort()
	// Try exact container port mapping
	if p := inst.resolvePort(fmt.Sprintf("%d/tcp", containerPort)); p != 0 {
		return p
	}
	// Try common SGLang ports
	for _, key := range []string{"8000/tcp", "18000/tcp", "30000/tcp"} {
		if p := inst.resolvePort(key); p != 0 {
			return p
		}
	}
	// Fallback: direct port range
	if inst.DirectPortStart != nil {
		return *inst.DirectPortStart
	}
	return 0
}

// ResolveDirectSSHPort resolves the direct SSH host port (22/tcp mapping).
func (inst *Instance) ResolveDirectSSHPort() int {
	return inst.resolvePort("22/tcp")
}

func (inst *Instance) resolvePort(key string) int {
	mappings, ok := inst.Ports[key]
	if !ok || len(mappings) == 0 {
		return 0
	}
	p, err := strconv.Atoi(mappings[0].HostPort)
	if err != nil {
		return 0
	}
	return p
}

// ParseExtraEnv parses the extra_env field which can be either a list of pairs
// [["KEY","VALUE"], ...] or a dict {"KEY": "VALUE"}.
func (inst *Instance) ParseExtraEnv() map[string]string {
	env := make(map[string]string)
	if inst.ExtraEnv == nil {
		return env
	}

	// Try dict format first
	var dictEnv map[string]string
	if err := json.Unmarshal(inst.ExtraEnv, &dictEnv); err == nil {
		return dictEnv
	}

	// Try list of pairs format [["KEY","VALUE"], ...]
	var listEnv [][]string
	if err := json.Unmarshal(inst.ExtraEnv, &listEnv); err == nil {
		for _, pair := range listEnv {
			if len(pair) >= 2 && pair[0] != "" && pair[0][0] != '-' {
				env[pair[0]] = pair[1]
			}
		}
		return env
	}

	return env
}

// DisplayName returns a human-readable name for the instance.
func (inst *Instance) DisplayName() string {
	name := fmt.Sprintf("#%d %sx%d", inst.ID, inst.GPUName, inst.NumGPUs)
	if inst.Label != "" {
		name += " (" + inst.Label + ")"
	}
	return name
}

// InstanceEvent is emitted by the Watcher when instance state changes.
type InstanceEvent struct {
	Type     string
	Instance *Instance
}
