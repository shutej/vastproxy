package backend

import (
	"fmt"
	"strconv"
	"strings"
)

// GPUMetrics holds parsed nvidia-smi output.
type GPUMetrics struct {
	Utilization float64
	Temperature float64
}

// ParseNvidiaSmi parses the output of:
//
//	nvidia-smi --query-gpu=utilization.gpu,temperature.gpu --format=csv,noheader,nounits
//
// which produces lines like "98, 73".
func ParseNvidiaSmi(output string) (*GPUMetrics, error) {
	// Take first line (first GPU or averaged).
	line := strings.TrimSpace(strings.SplitN(output, "\n", 2)[0])
	if line == "" {
		return nil, fmt.Errorf("empty nvidia-smi output")
	}

	parts := strings.SplitN(line, ",", 2)
	if len(parts) < 2 {
		return nil, fmt.Errorf("unexpected nvidia-smi format: %q", line)
	}

	util, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return nil, fmt.Errorf("parse utilization: %w", err)
	}
	temp, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return nil, fmt.Errorf("parse temperature: %w", err)
	}

	return &GPUMetrics{
		Utilization: util,
		Temperature: temp,
	}, nil
}
