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

// InstanceHealthChangedMsg is sent when an instance's health state changes.
type InstanceHealthChangedMsg struct {
	InstanceID int
	State      vast.InstanceState
	ModelName  string
}

// GPUMetricsMsg delivers GPU metrics from a backend's health loop.
type GPUMetricsMsg struct {
	backend.GPUUpdate
}

// ServerStartedMsg is sent when the HTTP server starts listening.
type ServerStartedMsg struct {
	ListenAddr string
}

// ErrorMsg is sent when a fatal error occurs.
type ErrorMsg struct {
	Error error
}

// TickMsg is sent periodically to refresh durations in the view.
type TickMsg time.Time
