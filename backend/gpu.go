package backend

import (
	"fmt"
	"strconv"
	"strings"
)

// GPUMetric holds parsed nvidia-smi output for a single GPU.
type GPUMetric struct {
	Utilization float64
	Temperature float64
}

// GPUMetrics holds parsed nvidia-smi output for all GPUs on an instance.
type GPUMetrics struct {
	GPUs []GPUMetric
}

// AvgUtilization returns the mean utilization across all GPUs.
func (m *GPUMetrics) AvgUtilization() float64 {
	if len(m.GPUs) == 0 {
		return 0
	}
	var sum float64
	for _, g := range m.GPUs {
		sum += g.Utilization
	}
	return sum / float64(len(m.GPUs))
}

// AvgTemperature returns the mean temperature across all GPUs.
func (m *GPUMetrics) AvgTemperature() float64 {
	if len(m.GPUs) == 0 {
		return 0
	}
	var sum float64
	for _, g := range m.GPUs {
		sum += g.Temperature
	}
	return sum / float64(len(m.GPUs))
}

// ParseNvidiaSmi parses the output of:
//
//	nvidia-smi --query-gpu=utilization.gpu,temperature.gpu --format=csv,noheader,nounits
//
// which produces one line per GPU like "98, 73".
func ParseNvidiaSmi(output string) (*GPUMetrics, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, fmt.Errorf("empty nvidia-smi output")
	}

	lines := strings.Split(output, "\n")
	var gpus []GPUMetric
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
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

		gpus = append(gpus, GPUMetric{Utilization: util, Temperature: temp})
	}

	if len(gpus) == 0 {
		return nil, fmt.Errorf("no GPU data parsed")
	}

	return &GPUMetrics{GPUs: gpus}, nil
}
