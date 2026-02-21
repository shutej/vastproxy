package tui

import (
	"time"
	"vastproxy/backend"
	"vastproxy/vast"
)

// InstanceAddedMsg is sent when a new instance is discovered.
type InstanceAddedMsg struct {
	Instance *vast.Instance
}

// InstanceUpdatedMsg is sent when an instance's metrics are updated.
type InstanceUpdatedMsg struct {
	Instance *vast.Instance
}

// InstanceRemovedMsg is sent when an instance is removed from vast.ai.
type InstanceRemovedMsg struct {
	InstanceID int
}

// GPUMetricsMsg delivers GPU metrics from a backend's health loop.
type GPUMetricsMsg struct {
	backend.GPUUpdate
}

// TickMsg is sent periodically to refresh durations in the view.
type TickMsg time.Time

// AbortClearedMsg clears the abort status message after a delay.
type AbortClearedMsg struct{}

// DestroyClearedMsg clears the destroy status message after a delay.
type DestroyClearedMsg struct{}
