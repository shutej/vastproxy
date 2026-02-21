package backend

import (
	"testing"
)

func TestParseNvidiaSmi(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantU   float64
		wantT   float64
		wantErr bool
	}{
		{
			name:  "normal",
			input: "98, 73",
			wantU: 98, wantT: 73,
		},
		{
			name:  "with whitespace",
			input: "  42 ,  65  \n",
			wantU: 42, wantT: 65,
		},
		{
			name:  "zero values",
			input: "0, 0",
			wantU: 0, wantT: 0,
		},
		{
			name:  "decimal values",
			input: "55.5, 72.3",
			wantU: 55.5, wantT: 72.3,
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
		{
			name:    "whitespace only",
			input:   "   \n",
			wantErr: true,
		},
		{
			name:    "single value",
			input:   "98",
			wantErr: true,
		},
		{
			name:    "non-numeric",
			input:   "abc, def",
			wantErr: true,
		},
		{
			name:    "first non-numeric",
			input:   "abc, 73",
			wantErr: true,
		},
		{
			name:    "second non-numeric",
			input:   "73, abc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseNvidiaSmi(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(m.GPUs) != 1 {
				t.Fatalf("GPUs len = %d, want 1", len(m.GPUs))
			}
			if m.GPUs[0].Utilization != tt.wantU {
				t.Errorf("Utilization = %f, want %f", m.GPUs[0].Utilization, tt.wantU)
			}
			if m.GPUs[0].Temperature != tt.wantT {
				t.Errorf("Temperature = %f, want %f", m.GPUs[0].Temperature, tt.wantT)
			}
		})
	}
}

func TestParseNvidiaSmiMultiGPU(t *testing.T) {
	input := "98, 73\n45, 60\n12, 55\n"
	m, err := ParseNvidiaSmi(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.GPUs) != 3 {
		t.Fatalf("GPUs len = %d, want 3", len(m.GPUs))
	}

	want := []GPUMetric{
		{Utilization: 98, Temperature: 73},
		{Utilization: 45, Temperature: 60},
		{Utilization: 12, Temperature: 55},
	}
	for i, g := range m.GPUs {
		if g != want[i] {
			t.Errorf("GPU[%d] = %+v, want %+v", i, g, want[i])
		}
	}
}

func TestParseNvidiaSmiMultiGPUWithBlankLines(t *testing.T) {
	input := "98, 73\n\n45, 60\n"
	m, err := ParseNvidiaSmi(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.GPUs) != 2 {
		t.Fatalf("GPUs len = %d, want 2", len(m.GPUs))
	}
}
