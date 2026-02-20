package tui

import (
	"log"
	"slices"
	"strings"
	"time"
	"vastproxy/backend"
	"vastproxy/vast"

	tea "github.com/charmbracelet/bubbletea"
)

// Model is the bubbletea model for the proxy TUI.
type Model struct {
	instances    map[int]*InstanceView
	order        []int // instance IDs in discovery order
	listenAddr   string
	err          error
	eventCh      <-chan vast.InstanceEvent
	gpuCh        <-chan backend.GPUUpdate
	startWatcher func() // called once from Init to start the watcher
	started      bool
}

// NewModel creates the TUI model.
func NewModel(eventCh <-chan vast.InstanceEvent, gpuCh <-chan backend.GPUUpdate, listenAddr string, startWatcher func()) Model {
	return Model{
		instances:    make(map[int]*InstanceView),
		eventCh:      eventCh,
		gpuCh:        gpuCh,
		listenAddr:   listenAddr,
		startWatcher: startWatcher,
	}
}

// Init returns the initial commands.
func (m Model) Init() tea.Cmd {
	// Start the watcher now that the TUI is ready to receive messages.
	if m.startWatcher != nil && !m.started {
		m.startWatcher()
	}
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
		log.Printf("tui: InstanceAddedMsg id=%d name=%s", msg.Instance.ID, msg.Instance.DisplayName())
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
		if !m.hasID(msg.Instance.ID) {
			m.order = append(m.order, msg.Instance.ID)
		}
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
		return m, waitForEvent(m.eventCh)

	case GPUMetricsMsg:
		if iv, ok := m.instances[msg.InstanceID]; ok {
			iv.GPUUtil = msg.Utilization
			iv.GPUTemp = msg.Temperature
		}
		return m, waitForGPU(m.gpuCh)

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

	// Count healthy/total from our instance views.
	total := len(m.instances)
	healthy := 0
	for _, iv := range m.instances {
		if iv.State == vast.StateHealthy {
			healthy++
		}
	}

	// Header.
	b.WriteString(RenderHeader(m.listenAddr, total, healthy))
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

func (m *Model) hasID(id int) bool {
	return slices.Contains(m.order, id)
}

// waitForEvent returns a command that waits for the next watcher event.
func waitForEvent(ch <-chan vast.InstanceEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return nil
		}
		log.Printf("tui: received event type=%s instance=%d", evt.Type, evt.Instance.ID)
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
