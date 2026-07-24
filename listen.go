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
type listenActionResult struct {
	name string
	err  error
}

type listenActionMsg struct {
	action  string
	results []listenActionResult
}

type listenConfirm struct {
	action string
	names  []string
	yes    bool
}

type listenModel struct {
	ctx          context.Context
	cancel       context.CancelFunc
	conn         net.Conn
	decoder      *json.Decoder
	version      uint64
	items        map[string]daemon.InventoryItem
	changed      map[string]time.Time
	recent       []string
	recentAt     time.Time
	selected     int
	marked       map[string]bool
	visual       bool
	visualAnchor int
	visualBase   map[string]bool
	gPending     bool
	width        int
	height       int
	connected    bool
	expanded     bool
	help         bool
	confirm      *listenConfirm
	action       string
	notice       string
	noticeAt     time.Time
	err          error
	quitting     bool
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
		marked:  make(map[string]bool),
	}
}

func (m listenModel) Init() tea.Cmd {
	return tea.Batch(connectListener(m.ctx), listenTick())
}

func connectListener(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		timeout := 5 * time.Second
		conn, err := connectDaemon(timeout)
		if err != nil {
			return listenErrMsg{err}
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

func runListenActions(action string, names []string) tea.Cmd {
	return func() tea.Msg {
		results := make([]listenActionResult, 0, len(names))
		for _, name := range names {
			_, err := rpcCallRaw(action, nameP(name))
			results = append(results, listenActionResult{name: name, err: err})
		}
		return listenActionMsg{action: action, results: results}
	}
}

func (m listenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		key := msg.String()
		if m.confirm != nil {
			return m.updateConfirm(key)
		}
		if m.help {
			if key == "?" || key == "esc" || key == "q" {
				m.help = false
			}
			return m, nil
		}
		if key != "g" {
			m.gPending = false
		}

		switch key {
		case "q", "ctrl+c":
			m.quitting = true
			m.cancel()
			if m.conn != nil {
				m.conn.Close()
			}
			return m, tea.Quit
		case "esc":
			m.clearSelection()
		case "up", "k":
			m.moveCursor(-1)
		case "down", "j":
			m.moveCursor(1)
		case "h", "left":
			m.expanded = false
		case "l", "right":
			if len(m.items) > 0 {
				m.expanded = true
			}
		case "home":
			m.selected = 0
			m.updateVisualSelection()
		case "end", "G":
			if len(m.items) > 0 {
				m.selected = len(m.items) - 1
				m.updateVisualSelection()
			}
		case "g":
			if m.gPending {
				m.selected = 0
				m.gPending = false
				m.updateVisualSelection()
			} else {
				m.gPending = true
			}
		case "enter":
			if len(m.items) > 0 {
				m.expanded = !m.expanded
			}
		case " ":
			m.toggleCurrentSelection()
		case "v":
			m.toggleVisualSelection()
		case "?":
			m.help = true
		case "e":
			return m.beginAction("exec")
		case "s":
			return m.beginAction("stop")
		case "+", "=":
			return m.beginAction("enable")
		case "-":
			return m.beginAction("disable")
		case "d", "delete":
			names := m.targetNames()
			if len(names) > 0 && m.action == "" {
				m.confirm = &listenConfirm{action: "delete", names: names}
			}
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
		var failed []string
		for _, result := range msg.results {
			if result.err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", result.name, result.err))
			}
		}
		if len(failed) > 0 {
			m.notice = fmt.Sprintf("%s: %d/%d failed: %s", msg.action, len(failed), len(msg.results), strings.Join(failed, "; "))
		} else {
			m.notice = fmt.Sprintf("%s complete for %d angl(s)", msg.action, len(msg.results))
			m.clearSelection()
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

func (m listenModel) updateConfirm(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "n", "N":
		m.confirm = nil
	case "h", "left":
		m.confirm.yes = true
	case "l", "right":
		m.confirm.yes = false
	case "tab":
		m.confirm.yes = !m.confirm.yes
	case "y", "Y":
		m.confirm.yes = true
	case "enter":
		if !m.confirm.yes {
			m.confirm = nil
			return m, nil
		}
		action := m.confirm.action
		names := append([]string(nil), m.confirm.names...)
		m.confirm = nil
		return m.beginActionWithNames(action, names)
	}
	return m, nil
}

func (m listenModel) beginAction(action string) (tea.Model, tea.Cmd) {
	return m.beginActionWithNames(action, m.targetNames())
}

func (m listenModel) beginActionWithNames(action string, names []string) (tea.Model, tea.Cmd) {
	if m.action != "" || len(names) == 0 {
		return m, nil
	}
	m.action = action
	m.notice = fmt.Sprintf("%s %d angl(s)…", action, len(names))
	return m, runListenActions(action, names)
}

func (m *listenModel) sortedNames() []string {
	names := make([]string, 0, len(m.items))
	for name := range m.items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *listenModel) targetNames() []string {
	var names []string
	for name := range m.marked {
		if m.marked[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		if name := m.selectedName(); name != "" {
			names = []string{name}
		}
	}
	return names
}

func (m *listenModel) clearSelection() {
	m.marked = make(map[string]bool)
	m.visual = false
	m.visualBase = nil
}

func (m *listenModel) toggleCurrentSelection() {
	name := m.selectedName()
	if name == "" {
		return
	}
	if m.marked[name] {
		delete(m.marked, name)
	} else {
		m.marked[name] = true
	}
	m.visual = false
	m.visualBase = nil
}

func (m *listenModel) toggleVisualSelection() {
	if len(m.items) == 0 {
		return
	}
	if m.visual {
		m.visual = false
		m.visualBase = nil
		return
	}
	m.visual = true
	m.visualAnchor = m.selected
	m.visualBase = make(map[string]bool, len(m.marked))
	for name, selected := range m.marked {
		m.visualBase[name] = selected
	}
	m.updateVisualSelection()
}

func (m *listenModel) moveCursor(delta int) {
	m.selected += delta
	m.clampSelection()
	m.updateVisualSelection()
}

func (m *listenModel) updateVisualSelection() {
	if !m.visual {
		return
	}
	names := m.sortedNames()
	if len(names) == 0 {
		return
	}
	m.marked = make(map[string]bool, len(m.visualBase)+len(names))
	for name, selected := range m.visualBase {
		if selected {
			m.marked[name] = true
		}
	}
	lo, hi := m.visualAnchor, m.selected
	if lo > hi {
		lo, hi = hi, lo
	}
	lo = max(0, lo)
	hi = min(len(names)-1, hi)
	for index := lo; index <= hi; index++ {
		m.marked[names[index]] = true
	}
}

func (m *listenModel) pruneSelection() {
	for name := range m.marked {
		if _, ok := m.items[name]; !ok {
			delete(m.marked, name)
		}
	}
	for name := range m.visualBase {
		if _, ok := m.items[name]; !ok {
			delete(m.visualBase, name)
		}
	}
	if len(m.items) == 0 {
		m.visual = false
	}
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
	m.pruneSelection()
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
		m.pruneSelection()
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
	if m.selected < 0 {
		m.selected = 0
	}
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

	names := m.sortedNames()
	store := catalog.New()
	statuses := make([]daemon.ProcessStatus, 0, len(names))
	markedDisplayNames := make(map[string]bool)
	for _, name := range names {
		item := m.items[name]
		status := item.ProcessStatus
		changeMark := "  "
		if _, changed := m.changed[name]; changed {
			changeMark = "* "
		}
		selectionMark := "  "
		if m.marked[name] {
			selectionMark = "> "
		}
		displayName := selectionMark + changeMark + status.Name
		status.Name = displayName
		statuses = append(statuses, status)
		store.Labels[displayName] = item.Metadata
		if m.marked[name] {
			markedDisplayNames[displayName] = true
		}
	}
	body := renderListTableWithSelection(statuses, store, width, m.selected, markedDisplayNames)
	if m.expanded {
		if name := m.selectedName(); name != "" {
			body += "\n" + renderListDetail(m.items[name], width)
		}
	}
	if m.help {
		body = renderListenHelp(width)
	}
	if m.confirm != nil {
		body = renderListenConfirm(*m.confirm, width)
	}

	mode := "normal"
	if m.visual {
		mode = "visual"
	}
	footer := dimStyle.Render(fmt.Sprintf("%s • %d selected • j/k move h/l details • space/v select • e exec s stop +/- enable • d delete • ? help • q quit", mode, len(m.marked)))
	if m.notice != "" {
		footer = m.notice + "\n" + footer
	}
	if len(m.recent) > 0 {
		footer = "changes: " + strings.Join(m.recent, ", ") + "\n" + footer
	}
	return header + "\n\n" + body + "\n" + footer
}

func renderListenHelp(width int) string {
	text := `Keyboard help

  j/k or arrows  move cursor       h/l          collapse/expand details
  gg / G         first/last row    Enter        toggle details
  Space          toggle row        v            visual range selection
  Esc            clear selection   ?            close help
  e              exec now          s            stop running
  + / -          enable/disable    d or Delete  delete with confirmation
  q / Ctrl-C     quit`
	return listenModalStyle(width).Render(text)
}

func renderListenConfirm(confirm listenConfirm, width int) string {
	yes, no := " Yes ", " No "
	selected := lipgloss.NewStyle().Reverse(true)
	if confirm.yes {
		yes = selected.Render(yes)
	} else {
		no = selected.Render(no)
	}
	prompt := fmt.Sprintf("Are you sure you want to delete %d angl(s)?\n\n%s  %s\n\nUse h/l and Enter; Esc cancels.", len(confirm.names), yes, no)
	return listenModalStyle(width).Render(prompt)
}

func listenModalStyle(width int) lipgloss.Style {
	modalWidth := max(10, min(72, width-8))
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 2).
		Width(modalWidth)
}
