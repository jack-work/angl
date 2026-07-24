//go:build windows

package main

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jack-work/angl/catalog"
	"github.com/jack-work/angl/daemon"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// renderListTable keeps every rendered line within width. JSON output remains
// lossless; the human table progressively drops secondary columns and snips
// cells as the terminal narrows.
func renderListTable(statuses []daemon.ProcessStatus, store catalog.Store, width int) string {
	return renderListTableWithSelection(statuses, store, width, -1, nil)
}

func renderListTableWithSelection(statuses []daemon.ProcessStatus, store catalog.Store, width, selected int, marked map[string]bool) string {
	t := table.NewWriter()
	t.SetStyle(table.StyleRounded)
	t.AppendHeader(table.Row{"NAME", "STATE", "PID", "UPTIME", "RESTARTS", "COMMAND", "CHARGE", "METADATA"})
	selectedName := ""
	for index, status := range statuses {
		if index == selected {
			selectedName = status.Name
		}
		pid := "-"
		if status.PID > 0 {
			pid = strconv.Itoa(status.PID)
		}
		uptime := status.Uptime
		if uptime == "" {
			uptime = status.Lifetime
		}
		if uptime == "" {
			uptime = "-"
		}
		restarts := strconv.Itoa(status.Restarts)
		if status.MaxRestarts > 0 {
			restarts = strconv.Itoa(status.Restarts) + "/" + strconv.Itoa(status.MaxRestarts)
		}
		state := string(status.State)
		switch status.State {
		case daemon.StateRunning:
			state = text.FgGreen.Sprint(state)
		case daemon.StateFailed:
			state = text.FgRed.Sprint(state)
		case daemon.StateBackoff:
			state = text.FgYellow.Sprint(state)
		case daemon.StateDisabled:
			state = text.Faint.Sprint(state)
		}
		name := sanitizeCell(status.Name, 0)
		if strings.HasPrefix(name, "* ") {
			name = text.FgHiYellow.Sprint(name)
		}
		t.AppendRow(table.Row{
			name, state, pid, uptime, restarts,
			sanitizeCell(formatCommand(status.Command, status.Args), 0),
			sanitizeCell(status.Charge, 0), formatLabels(store.Labels[status.Name]),
		})
	}
	if selectedName != "" || len(marked) > 0 {
		t.SetRowPainter(func(row table.Row) text.Colors {
			name, ok := row[0].(string)
			if !ok {
				return nil
			}
			plainName := text.StripEscape(name)
			if marked[plainName] {
				return text.Colors{text.BgBlue, text.FgHiWhite}
			}
			if plainName == sanitizeCell(selectedName, 0) {
				return text.Colors{text.BgHiBlack}
			}
			return nil
		})
	}

	configs := listColumnConfigs(width)
	t.SetColumnConfigs(configs)
	if width > 0 {
		t.Style().Size.WidthMax = width
	}
	return t.Render()
}

// renderListDetail shows the selected record using the same responsive column
// set as the inventory table. Unlike the compact rows, every visible value is
// retained and wrapped within its column.
func renderListDetail(item daemon.InventoryItem, width int) string {
	status := item.ProcessStatus
	pid := "-"
	if status.PID > 0 {
		pid = strconv.Itoa(status.PID)
	}
	uptime := status.Uptime
	if status.State == daemon.StateBackoff && status.NextRunIn != "" {
		uptime = "next " + status.NextRunIn
	} else if uptime == "" {
		uptime = status.Lifetime
	}
	if uptime == "" {
		uptime = "-"
	}
	restarts := strconv.Itoa(status.Restarts)
	if status.MaxRestarts > 0 {
		restarts += "/" + strconv.Itoa(status.MaxRestarts)
	}

	t := table.NewWriter()
	t.SetStyle(table.StyleRounded)
	t.AppendHeader(table.Row{"NAME", "STATE", "PID", "UPTIME", "RESTARTS", "COMMAND", "CHARGE", "METADATA"})
	t.AppendRow(table.Row{
		sanitizeCell(status.Name, 0), sanitizeCell(string(status.State), 0), pid, uptime, restarts,
		sanitizeCell(formatCommand(status.Command, status.Args), 0),
		sanitizeCell(status.Charge, 0), formatLabelsFull(item.Metadata),
	})
	configs := listColumnConfigs(width)
	for index := range configs {
		if !configs[index].Hidden {
			configs[index].WidthMaxEnforcer = text.WrapHard
		}
	}
	t.SetColumnConfigs(configs)
	if width > 0 {
		t.Style().Size.WidthMax = width
	}
	return t.Render()
}

func formatLabelsFull(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return sanitizeCell(strings.Join(parts, ","), 0)
}

func listColumnConfigs(width int) []table.ColumnConfig {
	configs := []table.ColumnConfig{
		{Number: 1, WidthMax: 30, WidthMaxEnforcer: snipTableCell},
		{Number: 2, WidthMax: 10, WidthMaxEnforcer: snipTableCell},
		{Number: 3, Align: text.AlignRight},
		{Number: 4, Align: text.AlignRight},
		{Number: 5, Align: text.AlignRight},
		{Number: 6, WidthMax: 48, WidthMaxEnforcer: snipTableCell},
		{Number: 7, WidthMax: 42, WidthMaxEnforcer: snipTableCell},
		{Number: 8, WidthMax: 36, WidthMaxEnforcer: snipTableCell},
	}
	if width <= 0 {
		return configs
	}

	set := func(number, maxWidth int, hidden bool) {
		config := &configs[number-1]
		config.WidthMax = maxWidth
		config.Hidden = hidden
		config.WidthMaxEnforcer = snipTableCell
	}

	switch {
	case width < 48:
		// Two columns cost seven characters in borders, separators, and padding.
		set(1, max(4, width-15), false)
		set(2, 8, false)
		for number := 3; number <= 8; number++ {
			set(number, 0, true)
		}
	case width < 72:
		// Three columns cost ten characters of table framing.
		set(1, 14, false)
		set(2, 8, false)
		set(3, 0, true)
		set(4, 0, true)
		set(5, 0, true)
		set(6, max(7, width-32), false)
		set(7, 0, true)
		set(8, 0, true)
	case width < 100:
		set(1, 18, false)
		set(2, 8, false)
		set(3, 5, false)
		set(4, 0, true)
		set(5, 0, true)
		set(6, max(7, width-44), false)
		set(7, 0, true)
		set(8, 0, true)
	case width < 130:
		set(1, 22, false)
		set(2, 8, false)
		set(3, 6, false)
		set(4, 10, false)
		set(5, 0, true)
		set(6, max(7, width-62), false)
		set(7, 0, true)
		set(8, 0, true)
	case width < 170:
		set(1, 24, false)
		set(2, 8, false)
		set(3, 6, false)
		set(4, 10, false)
		set(5, 8, false)
		set(6, max(7, width-98), false)
		set(7, 20, false)
		set(8, 0, true)
	default:
		set(1, 26, false)
		set(2, 8, false)
		set(3, 6, false)
		set(4, 10, false)
		set(5, 8, false)
		set(6, 40, false)
		set(7, 30, false)
		set(8, max(8, width-153), false)
	}
	return configs
}
