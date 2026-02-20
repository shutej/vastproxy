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
			name:  "multiline takes first",
			input: "10, 20\n30, 40",
			wantU: 10, wantT: 20,
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
			if m.Utilization != tt.wantU {
				t.Errorf("Utilization = %f, want %f", m.Utilization, tt.wantU)
			}
			if m.Temperature != tt.wantT {
				t.Errorf("Temperature = %f, want %f", m.Temperature, tt.wantT)
			}
		})
	}
}
