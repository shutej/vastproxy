package tui

import (
	"fmt"
	"strings"
	"time"
	"vastproxy/backend"
	"vastproxy/vast"

	"github.com/charmbracelet/lipgloss"
)

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39"))

	healthyDot  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("46")).Render("●")
	unhealthyDot = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")).Render("●")

	stateHealthy   = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))
	stateUnhealthy = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	stateConnecting = lipgloss.NewStyle().Foreground(lipgloss.Color("226"))
	stateRemoving  = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	stateDim       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	gpuBarFull  = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))
	gpuBarWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("226"))
	gpuBarHot   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	gpuBarEmpty = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// RenderHeader renders the proxy status header line.
func RenderHeader(listenAddr string, totalBackends, healthyBackends int) string {
	return headerStyle.Render(fmt.Sprintf(
		"Listening on %s | %d backends (%d healthy)",
		listenAddr, totalBackends, healthyBackends,
	))
}

// InstanceView holds the display state for a single instance.
type InstanceView struct {
	ID            int
	GPUName       string
	NumGPUs       int
	State         vast.InstanceState
	StateSince    time.Time
	ModelName     string
	GPUUtil       float64   // average utilization (used for API fallback display)
	GPUTemp       float64   // average temperature (used for API fallback display)
	PerGPU        []backend.GPUMetric // per-GPU metrics from SSH nvidia-smi
	HasSSHMetrics bool      // true once we've received GPU data via SSH; prevents API overwrite
}

// RenderInstance renders a multi-line view for a single instance.
func RenderInstance(iv *InstanceView) string {
	var lines []string

	// Line 1: #<id> <gpu>x<n>  STATE  duration
	duration := formatDuration(time.Since(iv.StateSince))
	stateStr := renderState(iv.State)
	lines = append(lines, fmt.Sprintf("  #%d %sx%d  %s  %s",
		iv.ID, iv.GPUName, iv.NumGPUs, stateStr, stateDim.Render(duration)))

	// Line 2: dot + model name
	dot := unhealthyDot
	if iv.State == vast.StateHealthy {
		dot = healthyDot
	}
	model := iv.ModelName
	if model == "" {
		model = "(discovering...)"
	}
	lines = append(lines, fmt.Sprintf("    %s %s", dot, model))

	// GPU bars: one per GPU if we have per-GPU data, otherwise a single aggregate bar.
	if len(iv.PerGPU) > 1 {
		for i, g := range iv.PerGPU {
			lines = append(lines, fmt.Sprintf("    GPU%d %s  %s",
				i, renderGPUBar(g.Utilization, 20),
				renderGPUStats(g.Utilization, g.Temperature)))
		}
	} else {
		lines = append(lines, fmt.Sprintf("    GPU  %s  %s",
			renderGPUBar(iv.GPUUtil, 20),
			renderGPUStats(iv.GPUUtil, iv.GPUTemp)))
	}

	return strings.Join(lines, "\n")
}

func renderState(s vast.InstanceState) string {
	switch s {
	case vast.StateHealthy:
		return stateHealthy.Render("HEALTHY")
	case vast.StateUnhealthy:
		return stateUnhealthy.Render("UNHEALTHY")
	case vast.StateConnecting:
		return stateConnecting.Render("CONNECTING")
	case vast.StateRemoving:
		return stateRemoving.Render("REMOVING")
	case vast.StateDiscovered:
		return stateConnecting.Render("DISCOVERED")
	default:
		return stateDim.Render(s.String())
	}
}

func renderGPUBar(utilPct float64, width int) string {
	frac := utilPct / 100.0
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))

	var style lipgloss.Style
	switch {
	case frac < 0.5:
		style = gpuBarFull
	case frac < 0.8:
		style = gpuBarWarn
	default:
		style = gpuBarHot
	}

	bar := style.Render(strings.Repeat("█", filled)) +
		gpuBarEmpty.Render(strings.Repeat("░", width-filled))
	return bar
}

func renderGPUStats(util, temp float64) string {
	return fmt.Sprintf("%.0f%%  %.0f°C", util, temp)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	return fmt.Sprintf("%dh%02dm", h, m)
}
