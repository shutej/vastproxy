package tui

import (
	"log"
	"slices"
	"strings"
	"time"
	"vastproxy/backend"
	"vastproxy/vast"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// StickyPercenter returns the sticky-request percentage for display.
type StickyPercenter interface {
	Percent() float64
}

// Model is the bubbletea model for the proxy TUI.
type Model struct {
	instances      map[int]*InstanceView
	order          []int // instance IDs in discovery order
	listenAddr     string
	err            error
	eventCh        <-chan vast.InstanceEvent
	gpuCh          <-chan backend.GPUUpdate
	startWatcher   func() // called once from Init to start the watcher
	abortFn        func() // called to abort all backend inference
	destroyFn      func() // called to destroy all vast.ai instances
	stickyStats    StickyPercenter
	started        bool
	width          int    // terminal width
	height         int    // terminal height
	scroll         int    // vertical scroll offset (in lines)
	confirmAbort   bool   // true when abort confirmation dialog is showing
	abortStatus    string // transient status message after abort
	confirmDestroy bool   // true when destroy confirmation dialog is showing
	destroyStatus  string // transient status message after destroy
}

// NewModel creates the TUI model.
func NewModel(eventCh <-chan vast.InstanceEvent, gpuCh <-chan backend.GPUUpdate, listenAddr string, startWatcher func(), abortFn func(), destroyFn func(), stickyStats StickyPercenter) Model {
	return Model{
		instances:    make(map[int]*InstanceView),
		eventCh:      eventCh,
		gpuCh:        gpuCh,
		listenAddr:   listenAddr,
		startWatcher: startWatcher,
		abortFn:      abortFn,
		destroyFn:    destroyFn,
		stickyStats:  stickyStats,
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

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.clampScroll()
		return m, nil

	case tea.KeyMsg:
		// When confirmation dialog is showing, only handle y/n/esc.
		if m.confirmAbort {
			switch msg.String() {
			case "y", "Y":
				m.confirmAbort = false
				m.abortStatus = "Aborting..."
				if m.abortFn != nil {
					go m.abortFn()
				}
				log.Printf("tui: user confirmed abort all")
				return m, clearAbortStatusAfter(3 * time.Second)
			case "n", "N", "esc":
				m.confirmAbort = false
				return m, nil
			}
			return m, nil
		}
		if m.confirmDestroy {
			switch msg.String() {
			case "y", "Y":
				m.confirmDestroy = false
				m.destroyStatus = "Destroying..."
				if m.destroyFn != nil {
					go m.destroyFn()
				}
				log.Printf("tui: user confirmed destroy all")
				return m, clearDestroyStatusAfter(3 * time.Second)
			case "n", "N", "esc":
				m.confirmDestroy = false
				return m, nil
			}
			return m, nil
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "a":
			m.confirmAbort = true
			return m, nil
		case "d":
			m.confirmDestroy = true
			return m, nil
		case "up", "k":
			m.scroll--
			m.clampScroll()
			return m, nil
		case "down", "j":
			m.scroll++
			m.clampScroll()
			return m, nil
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
			// Only use API-reported GPU metrics if we don't have SSH metrics yet.
			// SSH nvidia-smi data is fresher and per-backend; the vast.ai API
			// reports stale/aggregate values that overwrite correct readings.
			if !iv.HasSSHMetrics {
				if msg.Instance.GPUUtil != nil {
					iv.GPUUtil = *msg.Instance.GPUUtil
				}
				if msg.Instance.GPUTemp != nil {
					iv.GPUTemp = *msg.Instance.GPUTemp
				}
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
			iv.PerGPU = msg.GPUs
			// Compute averages for fallback / summary use.
			if len(msg.GPUs) > 0 {
				var sumU, sumT float64
				for _, g := range msg.GPUs {
					sumU += g.Utilization
					sumT += g.Temperature
				}
				iv.GPUUtil = sumU / float64(len(msg.GPUs))
				iv.GPUTemp = sumT / float64(len(msg.GPUs))
			}
			iv.HasSSHMetrics = true
			iv.SSHDirect = msg.IsDirect
		}
		return m, waitForGPU(m.gpuCh)

	case AbortClearedMsg:
		m.abortStatus = ""
		return m, nil

	case DestroyClearedMsg:
		m.destroyStatus = ""
		return m, nil

	case ErrorMsg:
		m.err = msg.Error
		return m, nil

	case TickMsg:
		// Purge instances that have been in REMOVING state for 30s+.
		now := time.Now()
		for id, iv := range m.instances {
			if iv.State == vast.StateRemoving && now.Sub(iv.StateSince) >= 30*time.Second {
				delete(m.instances, id)
				m.order = slices.DeleteFunc(m.order, func(x int) bool { return x == id })
			}
		}
		return m, tickCmd()
	}

	return m, nil
}

// View renders the TUI.
func (m Model) View() string {
	// --- Footer (pinned to bottom) ---
	var footer strings.Builder
	if m.err != nil {
		footer.WriteString("  ERROR: " + m.err.Error() + "\n")
	}
	if m.abortStatus != "" {
		footer.WriteString("  " + stateRemoving.Render(m.abortStatus) + "\n")
	}
	if m.destroyStatus != "" {
		footer.WriteString("  " + stateRemoving.Render(m.destroyStatus) + "\n")
	}
	if m.confirmAbort {
		footer.WriteString("  " + stateUnhealthy.Render("Abort all backend inference? (y/n)"))
	} else if m.confirmDestroy {
		footer.WriteString("  " + stateUnhealthy.Render("DESTROY all vast.ai instances? This is irreversible! (y/n)"))
	} else {
		footer.WriteString("  Press a to abort all | d to destroy all | q to quit")
	}
	footerStr := footer.String()
	footerLines := strings.Count(footerStr, "\n") + 1

	// --- Body (scrollable) ---
	var body strings.Builder

	// Count healthy/total from our instance views.
	total := len(m.instances)
	healthy := 0
	for _, iv := range m.instances {
		if iv.State == vast.StateHealthy {
			healthy++
		}
	}

	stickyPct := float64(-1)
	if m.stickyStats != nil {
		stickyPct = m.stickyStats.Percent()
	}
	body.WriteString(RenderHeader(m.listenAddr, total, healthy, stickyPct))
	body.WriteString("\n\n")

	// Collect rendered cards.
	var cards []string
	for _, id := range m.order {
		iv, ok := m.instances[id]
		if !ok {
			continue
		}
		cards = append(cards, RenderInstance(iv))
	}

	if len(cards) == 0 {
		body.WriteString("  Watching for vast.ai instances...\n")
	} else {
		body.WriteString(m.renderGrid(cards))
	}

	// Apply scroll to body, reserving space for the footer.
	bodyContent := body.String()
	scrolled := m.applyScroll(bodyContent, footerLines)

	return scrolled + "\n" + footerStr
}

func (m *Model) hasID(id int) bool {
	return slices.Contains(m.order, id)
}

// renderGrid lays out cards left-to-right, wrapping when they exceed terminal width.
func (m Model) renderGrid(cards []string) string {
	if len(cards) == 0 {
		return ""
	}

	termWidth := m.width
	if termWidth <= 0 {
		termWidth = 80
	}

	// Leave 1 column for the scrollbar.
	usable := termWidth - 1

	var rows []string
	var row []string
	rowWidth := 0
	gap := 2 // gap between cards in a row

	for _, card := range cards {
		cardW := lipgloss.Width(card)
		needed := cardW
		if len(row) > 0 {
			needed += gap
		}
		// Wrap to new row if this card doesn't fit (unless row is empty).
		if len(row) > 0 && rowWidth+needed > usable {
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, row...))
			row = nil
			rowWidth = 0
		}
		if len(row) > 0 {
			row = append(row, strings.Repeat(" ", gap))
			rowWidth += gap
		}
		row = append(row, card)
		rowWidth += cardW
	}
	if len(row) > 0 {
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, row...))
	}

	return strings.Join(rows, "\n\n")
}

// applyScroll slices visible lines from content and appends a scrollbar.
// reservedLines is the number of lines reserved for the fixed footer.
func (m Model) applyScroll(content string, reservedLines int) string {
	lines := strings.Split(content, "\n")
	totalLines := len(lines)

	viewH := m.height - reservedLines - 1 // -1 for the newline between body and footer
	if viewH <= 0 {
		return content // no window size yet, render everything
	}

	// If content fits, no scrolling needed.
	if totalLines <= viewH {
		// Pad to push footer to the bottom.
		pad := viewH - totalLines
		if pad > 0 {
			content += strings.Repeat("\n", pad)
		}
		return content
	}

	// Clamp scroll.
	maxScroll := totalLines - viewH
	offset := min(max(m.scroll, 0), maxScroll)

	visible := lines[offset:]
	if len(visible) > viewH {
		visible = visible[:viewH]
	}

	// Build scrollbar track for the right edge.
	thumbSize := max(1, viewH*viewH/totalLines)
	thumbPos := offset * (viewH - thumbSize) / maxScroll

	result := make([]string, len(visible))
	for i, line := range visible {
		ch := "│"
		if i >= thumbPos && i < thumbPos+thumbSize {
			ch = "┃"
		}
		result[i] = line + strings.Repeat(" ", max(0, m.width-1-lipgloss.Width(line))) + stateDim.Render(ch)
	}

	return strings.Join(result, "\n")
}

// clampScroll ensures scroll offset is within valid bounds.
func (m *Model) clampScroll() {
	if m.scroll < 0 {
		m.scroll = 0
	}
	// We can't know the exact max here (content isn't rendered yet),
	// but applyScroll will clamp it further. Just prevent negatives.
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

func clearAbortStatusAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return AbortClearedMsg{}
	})
}

func clearDestroyStatusAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return DestroyClearedMsg{}
	})
}
