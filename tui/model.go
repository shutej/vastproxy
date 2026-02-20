package tui

import (
	"strings"
	"time"
	"vastproxy/backend"
	"vastproxy/vast"

	tea "github.com/charmbracelet/bubbletea"
)

// Model is the bubbletea model for the proxy TUI.
type Model struct {
	instances  map[int]*InstanceView
	order      []int // instance IDs in discovery order
	listenAddr string
	totalBE    int
	healthyBE  int
	err        error
	eventCh    <-chan vast.InstanceEvent
	gpuCh      <-chan backend.GPUUpdate
}

// NewModel creates the TUI model.
func NewModel(eventCh <-chan vast.InstanceEvent, gpuCh <-chan backend.GPUUpdate) Model {
	return Model{
		instances: make(map[int]*InstanceView),
		eventCh:   eventCh,
		gpuCh:     gpuCh,
	}
}

// Init returns the initial commands.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		waitForEvent(m.eventCh),
		waitForGPU(m.gpuCh),
		tickCmd(),
	)
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case InstanceAddedMsg:
		iv := &InstanceView{
			ID:         msg.Instance.ID,
			GPUName:    msg.Instance.GPUName,
			NumGPUs:    msg.Instance.NumGPUs,
			State:      msg.Instance.State,
			StateSince: msg.Instance.StateChangedAt,
			ModelName:  msg.Instance.ModelName,
		}
		if msg.Instance.GPUUtil != nil {
			iv.GPUUtil = *msg.Instance.GPUUtil
		}
		if msg.Instance.GPUTemp != nil {
			iv.GPUTemp = *msg.Instance.GPUTemp
		}
		m.instances[msg.Instance.ID] = iv
		m.order = append(m.order, msg.Instance.ID)
		m.totalBE++
		return m, waitForEvent(m.eventCh)

	case InstanceUpdatedMsg:
		if iv, ok := m.instances[msg.Instance.ID]; ok {
			iv.State = msg.Instance.State
			iv.StateSince = msg.Instance.StateChangedAt
			if msg.Instance.ModelName != "" {
				iv.ModelName = msg.Instance.ModelName
			}
			if msg.Instance.GPUUtil != nil {
				iv.GPUUtil = *msg.Instance.GPUUtil
			}
			if msg.Instance.GPUTemp != nil {
				iv.GPUTemp = *msg.Instance.GPUTemp
			}
		}
		return m, waitForEvent(m.eventCh)

	case InstanceRemovedMsg:
		if iv, ok := m.instances[msg.InstanceID]; ok {
			iv.State = vast.StateRemoving
			iv.StateSince = time.Now()
		}
		m.totalBE--
		if m.totalBE < 0 {
			m.totalBE = 0
		}
		return m, waitForEvent(m.eventCh)

	case InstanceHealthChangedMsg:
		if iv, ok := m.instances[msg.InstanceID]; ok {
			iv.State = msg.State
			iv.StateSince = time.Now()
			if msg.ModelName != "" {
				iv.ModelName = msg.ModelName
			}
		}
		// Recount healthy.
		m.healthyBE = 0
		for _, iv := range m.instances {
			if iv.State == vast.StateHealthy {
				m.healthyBE++
			}
		}
		return m, nil

	case GPUMetricsMsg:
		if iv, ok := m.instances[msg.InstanceID]; ok {
			iv.GPUUtil = msg.Utilization
			iv.GPUTemp = msg.Temperature
		}
		return m, waitForGPU(m.gpuCh)

	case ServerStartedMsg:
		m.listenAddr = msg.ListenAddr
		return m, nil

	case ErrorMsg:
		m.err = msg.Error
		return m, nil

	case TickMsg:
		return m, tickCmd()
	}

	return m, nil
}

// View renders the TUI.
func (m Model) View() string {
	var b strings.Builder

	// Header.
	addr := m.listenAddr
	if addr == "" {
		addr = "(starting...)"
	}
	b.WriteString(RenderHeader(addr, m.totalBE, m.healthyBE))
	b.WriteString("\n\n")

	// Instances in discovery order.
	for _, id := range m.order {
		iv, ok := m.instances[id]
		if !ok {
			continue
		}
		b.WriteString(RenderInstance(iv))
		b.WriteString("\n\n")
	}

	if len(m.order) == 0 {
		b.WriteString("  Watching for vast.ai instances...\n")
	}

	if m.err != nil {
		b.WriteString("\n  ERROR: " + m.err.Error() + "\n")
	}

	b.WriteString("\n  Press q to quit\n")

	return b.String()
}

// waitForEvent returns a command that waits for the next watcher event.
func waitForEvent(ch <-chan vast.InstanceEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return nil
		}
		switch evt.Type {
		case "added":
			return InstanceAddedMsg{Instance: evt.Instance}
		case "updated":
			return InstanceUpdatedMsg{Instance: evt.Instance}
		case "removed":
			return InstanceRemovedMsg{InstanceID: evt.Instance.ID}
		}
		return nil
	}
}

// waitForGPU returns a command that waits for the next GPU metrics update.
func waitForGPU(ch <-chan backend.GPUUpdate) tea.Cmd {
	return func() tea.Msg {
		update, ok := <-ch
		if !ok {
			return nil
		}
		return GPUMetricsMsg{GPUUpdate: update}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}
