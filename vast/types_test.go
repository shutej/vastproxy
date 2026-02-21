package vast

import (
	"encoding/json"
	"testing"
)

func TestResolveContainerPort(t *testing.T) {
	tests := []struct {
		name     string
		extraEnv json.RawMessage
		onstart  string
		want     int
	}{
		{
			name:     "from SGLANG_ARGS in extra_env dict",
			extraEnv: json.RawMessage(`{"SGLANG_ARGS":"--port 9000 --model foo"}`),
			want:     9000,
		},
		{
			name:     "from SGLANG_ARGS in extra_env list",
			extraEnv: json.RawMessage(`[["SGLANG_ARGS","--port 7777"]]`),
			want:     7777,
		},
		{
			name:     "from VLLM_ARGS in extra_env dict",
			extraEnv: json.RawMessage(`{"VLLM_ARGS":"--port 9500 --model foo"}`),
			want:     9500,
		},
		{
			name:     "from VLLM_ARGS in extra_env list",
			extraEnv: json.RawMessage(`[["VLLM_ARGS","--port 7000"]]`),
			want:     7000,
		},
		{
			name:    "from onstart script",
			onstart: "python -m sglang --port 8888 --model bar",
			want:    8888,
		},
		{
			name: "default 8000",
			want: 8000,
		},
		{
			name:     "vllm default 18000",
			extraEnv: json.RawMessage(`{"VLLM_MODEL":"meta-llama/Llama-3-8B"}`),
			want:     18000,
		},
		{
			name:     "extra_env takes precedence over onstart",
			extraEnv: json.RawMessage(`{"SGLANG_ARGS":"--port 9000"}`),
			onstart:  "python -m sglang --port 8888",
			want:     9000,
		},
		{
			name:     "SGLANG_ARGS takes precedence over VLLM_ARGS",
			extraEnv: json.RawMessage(`{"SGLANG_ARGS":"--port 9000","VLLM_ARGS":"--port 7000"}`),
			want:     9000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &Instance{ExtraEnv: tt.extraEnv, Onstart: tt.onstart}
			if got := inst.ResolveContainerPort(); got != tt.want {
				t.Errorf("ResolveContainerPort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveDirectSSHPort(t *testing.T) {
	inst := Instance{
		Ports: map[string][]PortMapping{"22/tcp": {{HostPort: "22222"}}},
	}
	if got := inst.ResolveDirectSSHPort(); got != 22222 {
		t.Errorf("ResolveDirectSSHPort() = %d, want 22222", got)
	}

	inst2 := Instance{}
	if got := inst2.ResolveDirectSSHPort(); got != 0 {
		t.Errorf("ResolveDirectSSHPort() = %d, want 0", got)
	}
}

func TestParseExtraEnv(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want map[string]string
	}{
		{
			name: "dict format",
			raw:  json.RawMessage(`{"FOO":"bar","BAZ":"qux"}`),
			want: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name: "list format",
			raw:  json.RawMessage(`[["KEY1","val1"],["KEY2","val2"]]`),
			want: map[string]string{"KEY1": "val1", "KEY2": "val2"},
		},
		{
			name: "list skips flags",
			raw:  json.RawMessage(`[["-flag","val"],["KEY","val"]]`),
			want: map[string]string{"KEY": "val"},
		},
		{
			name: "nil",
			raw:  nil,
			want: map[string]string{},
		},
		{
			name: "invalid json",
			raw:  json.RawMessage(`not json`),
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &Instance{ExtraEnv: tt.raw}
			got := inst.ParseExtraEnv()
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestDisplayName(t *testing.T) {
	inst := Instance{ID: 123, GPUName: "RTX4090", NumGPUs: 2}
	if got := inst.DisplayName(); got != "#123 RTX4090x2" {
		t.Errorf("DisplayName() = %q", got)
	}

	inst.Label = "my-server"
	if got := inst.DisplayName(); got != "#123 RTX4090x2 (my-server)" {
		t.Errorf("DisplayName() = %q", got)
	}
}

func TestInstanceStateString(t *testing.T) {
	tests := []struct {
		state InstanceState
		want  string
	}{
		{StateDiscovered, "DISCOVERED"},
		{StateConnecting, "CONNECTING"},
		{StateHealthy, "HEALTHY"},
		{StateUnhealthy, "UNHEALTHY"},
		{StateRemoving, "REMOVING"},
		{InstanceState(99), "UNKNOWN"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestParseExtraEnvShortPair(t *testing.T) {
	// List format with pairs that have fewer than 2 elements should be skipped.
	inst := &Instance{ExtraEnv: json.RawMessage(`[["ONLY_KEY"],["K","V"]]`)}
	got := inst.ParseExtraEnv()
	if len(got) != 1 {
		t.Fatalf("got %v, want 1 entry", got)
	}
	if got["K"] != "V" {
		t.Errorf("K = %q, want V", got["K"])
	}
}

func TestParseExtraEnvEmptyKey(t *testing.T) {
	// Empty string key should be skipped.
	inst := &Instance{ExtraEnv: json.RawMessage(`[["","val"],["GOOD","ok"]]`)}
	got := inst.ParseExtraEnv()
	if len(got) != 1 {
		t.Fatalf("got %v, want 1 entry", got)
	}
}

func TestResolveEngineType(t *testing.T) {
	tests := []struct {
		name     string
		extraEnv json.RawMessage
		want     EngineType
	}{
		{
			name:     "sglang from SGLANG_ARGS",
			extraEnv: json.RawMessage(`{"SGLANG_ARGS":"--port 8000"}`),
			want:     EngineSGLang,
		},
		{
			name:     "vllm from VLLM_ARGS",
			extraEnv: json.RawMessage(`{"VLLM_ARGS":"--port 8000"}`),
			want:     EngineVLLM,
		},
		{
			name:     "vllm from VLLM_MODEL",
			extraEnv: json.RawMessage(`{"VLLM_MODEL":"meta-llama/Llama-3-8B"}`),
			want:     EngineVLLM,
		},
		{
			name:     "sglang takes precedence when both present",
			extraEnv: json.RawMessage(`{"SGLANG_ARGS":"--port 8000","VLLM_ARGS":"--port 9000"}`),
			want:     EngineSGLang,
		},
		{
			name: "unknown when no engine env vars",
			want: EngineUnknown,
		},
		{
			name:     "unknown with unrelated env vars",
			extraEnv: json.RawMessage(`{"FOO":"bar"}`),
			want:     EngineUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &Instance{ExtraEnv: tt.extraEnv}
			if got := inst.ResolveEngineType(); got != tt.want {
				t.Errorf("ResolveEngineType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEngineTypeString(t *testing.T) {
	tests := []struct {
		engine EngineType
		want   string
	}{
		{EngineSGLang, "sglang"},
		{EngineVLLM, "vllm"},
		{EngineUnknown, "unknown"},
		{EngineType(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.engine.String(); got != tt.want {
			t.Errorf("EngineType(%d).String() = %q, want %q", tt.engine, got, tt.want)
		}
	}
}

func TestEngineTypeSupportsAbort(t *testing.T) {
	tests := []struct {
		engine EngineType
		want   bool
	}{
		{EngineSGLang, true},
		{EngineVLLM, false},
		{EngineUnknown, false},
	}
	for _, tt := range tests {
		if got := tt.engine.SupportsAbort(); got != tt.want {
			t.Errorf("EngineType(%d).SupportsAbort() = %v, want %v", tt.engine, got, tt.want)
		}
	}
}
