//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jack-work/angl/catalog"
	"github.com/jack-work/angl/daemon"
	"golang.org/x/term"
)

const changedHighlightFor = 3 * time.Second

type listenConnectedMsg struct {
	conn         net.Conn
	decoder      *json.Decoder
	registration daemon.ListenRegistration
}

type listenUpdateMsg struct {
	update daemon.InventoryUpdate
}

type listenErrMsg struct{ err error }
type listenTickMsg time.Time
type listenActionMsg struct {
	action string
	name   string
	err    error
}

type listenModel struct {
	ctx       context.Context
	cancel    context.CancelFunc
	conn      net.Conn
	decoder   *json.Decoder
	version   uint64
	items     map[string]daemon.InventoryItem
	changed   map[string]time.Time
	recent    []string
	recentAt  time.Time
	selected  int
	width     int
	height    int
	connected bool
	expanded  bool
	action    string
	notice    string
	noticeAt  time.Time
	err       error
	quitting  bool
}

func cmdListenArgs(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("listen takes no arguments")
	}
	if !isInteractiveTerminal() {
		return fmt.Errorf("listen requires an interactive terminal")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	model := newListenModel(ctx, cancel)
	program := tea.NewProgram(model, tea.WithAltScreen())
	_, err := program.Run()
	return err
}

func isInteractiveTerminal() bool {
	return termIsTerminal(os.Stdin.Fd()) && termIsTerminal(os.Stdout.Fd())
}

// Kept behind this tiny seam so the listener model can be tested without a TTY.
var termIsTerminal = func(fd uintptr) bool { return term.IsTerminal(int(fd)) }

func newListenModel(ctx context.Context, cancel context.CancelFunc) listenModel {
	return listenModel{
		ctx:     ctx,
		cancel:  cancel,
		items:   make(map[string]daemon.InventoryItem),
		changed: make(map[string]time.Time),
	}
}

func (m listenModel) Init() tea.Cmd {
	return tea.Batch(connectListener(m.ctx), listenTick())
}

func connectListener(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		timeout := 5 * time.Second
		conn, err := winio.DialPipe(pipeName, &timeout)
		if err != nil {
			return listenErrMsg{fmt.Errorf("cannot reach daemon: %w", err)}
		}
		req := daemon.RPCRequest{JSONRPC: "2.0", Method: "listen", ID: 1}
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			conn.Close()
			return listenErrMsg{fmt.Errorf("register listener: %w", err)}
		}
		decoder := json.NewDecoder(conn)
		var resp daemon.RPCResponse
		if err := decoder.Decode(&resp); err != nil {
			conn.Close()
			return listenErrMsg{fmt.Errorf("read registration: %w", err)}
		}
		if resp.Error != nil {
			conn.Close()
			return listenErrMsg{fmt.Errorf("register listener: %s", resp.Error.Message)}
		}
		raw, err := json.Marshal(resp.Result)
		if err != nil {
			conn.Close()
			return listenErrMsg{err}
		}
		var registration daemon.ListenRegistration
		if err := json.Unmarshal(raw, &registration); err != nil {
			conn.Close()
			return listenErrMsg{fmt.Errorf("decode registration: %w", err)}
		}
		select {
		case <-ctx.Done():
			conn.Close()
			return listenErrMsg{ctx.Err()}
		default:
		}
		return listenConnectedMsg{conn: conn, decoder: decoder, registration: registration}
	}
}

func readListenerUpdate(decoder *json.Decoder) tea.Cmd {
	return func() tea.Msg {
		var notification daemon.RPCRequest
		if err := decoder.Decode(&notification); err != nil {
			if err == io.EOF {
				err = fmt.Errorf("daemon closed the listener")
			}
			return listenErrMsg{err}
		}
		if notification.Method != "list.update" {
			return listenErrMsg{fmt.Errorf("unexpected daemon notification %q", notification.Method)}
		}
		var update daemon.InventoryUpdate
		if err := json.Unmarshal(notification.Params, &update); err != nil {
			return listenErrMsg{fmt.Errorf("decode update: %w", err)}
		}
		return listenUpdateMsg{update: update}
	}
}

func listenTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return listenTickMsg(t) })
}

func runListenAction(action, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := rpcCallRaw(action, nameP(name))
		return listenActionMsg{action: action, name: name, err: err}
	}
}

func (m listenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			m.cancel()
			if m.conn != nil {
				m.conn.Close()
			}
			return m, tea.Quit
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(m.items)-1 {
				m.selected++
			}
		case "home", "g":
			m.selected = 0
		case "end", "G":
			if len(m.items) > 0 {
				m.selected = len(m.items) - 1
			}
		case "enter":
			if len(m.items) > 0 {
				m.expanded = !m.expanded
			}
		case "s":
			if m.action != "" {
				return m, nil
			}
			name := m.selectedName()
			if name == "" {
				return m, nil
			}
			action := "stop"
			if m.items[name].State == daemon.StateBackoff {
				action = "sing"
			}
			m.action = action
			m.notice = action + " " + name + "…"
			return m, runListenAction(action, name)
		case "d", "u":
			if m.action != "" {
				return m, nil
			}
			name := m.selectedName()
			if name == "" {
				return m, nil
			}
			action := map[string]string{"d": "disable", "u": "unregister"}[msg.String()]
			m.action = action
			m.notice = action + " " + name + "…"
			return m, runListenAction(action, name)
		}

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case listenConnectedMsg:
		m.conn = msg.conn
		m.decoder = msg.decoder
		m.connected = true
		m.err = nil
		m.applySnapshot(msg.registration.Snapshot, false)
		return m, readListenerUpdate(m.decoder)

	case listenUpdateMsg:
		if err := m.applyUpdate(msg.update); err != nil {
			m.err = err
			m.connected = false
			if m.conn != nil {
				m.conn.Close()
			}
			return m, nil
		}
		return m, readListenerUpdate(m.decoder)

	case listenErrMsg:
		if !m.quitting {
			m.err = msg.err
			m.connected = false
		}

	case listenActionMsg:
		m.action = ""
		m.noticeAt = time.Now()
		if msg.err != nil {
			m.notice = fmt.Sprintf("%s %s failed: %v", msg.action, msg.name, msg.err)
		} else {
			verb := map[string]string{
				"stop": "stopped", "sing": "sang", "disable": "disabled", "unregister": "unregistered",
			}[msg.action]
			m.notice = verb + " " + msg.name
		}

	case listenTickMsg:
		now := time.Time(msg)
		m.refreshElapsed(now)
		for name, changedAt := range m.changed {
			if now.Sub(changedAt) >= changedHighlightFor {
				delete(m.changed, name)
			}
		}
		if !m.recentAt.IsZero() && now.Sub(m.recentAt) >= changedHighlightFor {
			m.recent = nil
			m.recentAt = time.Time{}
		}
		if !m.noticeAt.IsZero() && now.Sub(m.noticeAt) >= changedHighlightFor {
			m.notice = ""
			m.noticeAt = time.Time{}
		}
		return m, listenTick()
	}
	return m, nil
}

func (m *listenModel) refreshElapsed(now time.Time) {
	for name, item := range m.items {
		if item.State == daemon.StateRunning && item.Started != "" {
			if started, err := time.Parse(time.RFC3339, item.Started); err == nil {
				item.Uptime = now.Sub(started).Round(time.Second).String()
			}
		}
		if item.CreatedAt != "" {
			if created, err := time.Parse(time.RFC3339, item.CreatedAt); err == nil {
				item.Lifetime = now.Sub(created).Round(time.Second).String()
			}
		}
		if item.State == daemon.StateBackoff && item.NextRun != "" {
			if nextRun, err := time.Parse(time.RFC3339, item.NextRun); err == nil {
				remaining := nextRun.Sub(now).Round(time.Second)
				if remaining > 0 {
					item.NextRunIn = remaining.String()
				} else {
					item.NextRunIn = "imminent"
				}
			}
		}
		m.items[name] = item
	}
}

func (m *listenModel) applySnapshot(snapshot daemon.InventorySnapshot, markChanges bool) {
	selectedName := m.selectedName()
	next := make(map[string]daemon.InventoryItem, len(snapshot.Items))
	now := time.Now()
	var recent []string
	for _, item := range snapshot.Items {
		next[item.Name] = item
		if markChanges {
			old, exists := m.items[item.Name]
			switch {
			case !exists:
				m.changed[item.Name] = now
				recent = append(recent, "+ "+item.Name)
			case !inventoryMeaningfullyEqual(old, item):
				m.changed[item.Name] = now
				if !reflect.DeepEqual(old.Metadata, item.Metadata) {
					recent = append(recent, "~ "+item.Name+" metadata")
				} else {
					recent = append(recent, "~ "+item.Name)
				}
			}
		}
	}
	if markChanges {
		for name := range m.items {
			if _, ok := next[name]; !ok {
				delete(m.changed, name)
				recent = append(recent, "- "+name)
			}
		}
		m.setRecent(recent, now)
	}
	m.items = next
	m.version = snapshot.Version
	m.restoreSelection(selectedName)
}

func (m *listenModel) applyUpdate(update daemon.InventoryUpdate) error {
	selectedName := m.selectedName()
	now := time.Now()
	switch update.Type {
	case "snapshot":
		if update.Snapshot == nil {
			return fmt.Errorf("daemon sent an empty snapshot update")
		}
		m.applySnapshot(*update.Snapshot, true)
		return nil
	case "patch":
		if update.Patch == nil {
			return fmt.Errorf("daemon sent an empty patch update")
		}
		patch := update.Patch
		if patch.BaseVersion != m.version {
			return fmt.Errorf("update gap: have version %d, patch starts at %d", m.version, patch.BaseVersion)
		}
		var recent []string
		for _, name := range patch.Removed {
			delete(m.items, name)
			delete(m.changed, name)
			recent = append(recent, "- "+name)
		}
		for _, item := range patch.Upsert {
			old, exists := m.items[item.Name]
			m.items[item.Name] = item
			switch {
			case !exists:
				m.changed[item.Name] = now
				recent = append(recent, "+ "+item.Name)
			case !inventoryMeaningfullyEqual(old, item):
				m.changed[item.Name] = now
				if !reflect.DeepEqual(old.Metadata, item.Metadata) {
					recent = append(recent, "~ "+item.Name+" metadata")
				} else {
					recent = append(recent, "~ "+item.Name)
				}
			}
		}
		m.setRecent(recent, now)
		m.version = patch.Version
		m.restoreSelection(selectedName)
		return nil
	default:
		return fmt.Errorf("unknown inventory update type %q", update.Type)
	}
}

func inventoryEqual(a, b daemon.InventoryItem) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func inventoryMeaningfullyEqual(a, b daemon.InventoryItem) bool {
	a.Uptime, b.Uptime = "", ""
	a.Lifetime, b.Lifetime = "", ""
	a.NextRunIn, b.NextRunIn = "", ""
	return inventoryEqual(a, b)
}

func (m *listenModel) setRecent(changes []string, now time.Time) {
	if len(changes) == 0 {
		return
	}
	m.recent = changes
	m.recentAt = now
}

func (m *listenModel) selectedName() string {
	names := make([]string, 0, len(m.items))
	for name := range m.items {
		names = append(names, name)
	}
	sort.Strings(names)
	if m.selected >= 0 && m.selected < len(names) {
		return names[m.selected]
	}
	return ""
}

func (m *listenModel) restoreSelection(name string) {
	if name != "" {
		names := make([]string, 0, len(m.items))
		for itemName := range m.items {
			names = append(names, itemName)
		}
		sort.Strings(names)
		for index, itemName := range names {
			if itemName == name {
				m.selected = index
				return
			}
		}
	}
	m.clampSelection()
}

func (m *listenModel) clampSelection() {
	if m.selected >= len(m.items) {
		m.selected = max(0, len(m.items)-1)
	}
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	dimStyle   = lipgloss.NewStyle().Faint(true)
	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func (m listenModel) View() string {
	if m.quitting {
		return ""
	}
	width := m.width
	if width <= 0 {
		width = 120
	}

	connection := "connecting"
	if m.connected {
		connection = "live"
	}
	header := titleStyle.Render("angl listen") + "  " + connection +
		fmt.Sprintf("  v%d  %d angls", m.version, len(m.items))
	if m.err != nil {
		header += "  " + errorStyle.Render(m.err.Error())
	}

	names := make([]string, 0, len(m.items))
	for name := range m.items {
		names = append(names, name)
	}
	sort.Strings(names)

	store := catalog.New()
	statuses := make([]daemon.ProcessStatus, 0, len(names))
	for _, name := range names {
		item := m.items[name]
		status := item.ProcessStatus
		displayName := status.Name
		if _, changed := m.changed[name]; changed {
			displayName = "* " + displayName
		} else {
			displayName = "  " + displayName
		}
		status.Name = displayName
		statuses = append(statuses, status)
		store.Labels[displayName] = item.Metadata
	}
	body := renderListTableWithSelection(statuses, store, width, m.selected)
	if m.expanded {
		if name := m.selectedName(); name != "" {
			body += "\n" + renderListDetail(m.items[name], width)
		}
	}

	sAction := "stop"
	if name := m.selectedName(); name != "" && m.items[name].State == daemon.StateBackoff {
		sAction = "sing"
	}
	footer := dimStyle.Render(fmt.Sprintf("↑/k ↓/j move • enter details • s %s • d disable • u unregister • q/esc quit", sAction))
	if m.notice != "" {
		footer = m.notice + "\n" + footer
	}
	if len(m.recent) > 0 {
		footer = "changes: " + strings.Join(m.recent, ", ") + "\n" + footer
	}
	return header + "\n\n" + body + "\n" + footer
}
