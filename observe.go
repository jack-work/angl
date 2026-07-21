//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jack-work/angl/catalog"
	"github.com/jack-work/angl/daemon"
	"github.com/jack-work/angl/internal/clix"
	"github.com/jack-work/angl/logcodec"
	"github.com/jack-work/angl/logstream"
)

func cmdTailArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("angl name required")
	}
	return cmdLogs(append([]string{args[0], "--follow"}, args[1:]...))
}

func cmdLogs(args []string) error {
	opts, explicit, err := clix.ParseLogArgs(args)
	if err != nil {
		return err
	}
	statuses, err := observationStatuses()
	if err != nil {
		return err
	}
	store, err := catalog.Load(catalog.DefaultPath())
	if err != nil {
		return err
	}
	names, err := resolveObservationNames(explicit, opts.Selector, opts.View, statuses, store)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	statusByName := make(map[string]daemon.ProcessStatus, len(statuses))
	for _, status := range statuses {
		statusByName[status.Name] = status
	}
	sources := make([]logstream.Source, 0, len(names))
	for _, name := range names {
		if err := daemon.ValidateName(name); err != nil {
			return err
		}
		if _, ok := statusByName[name]; !ok {
			return fmt.Errorf("unknown angl %q", name)
		}
		sources = append(sources, logstream.Source{Name: name, Path: daemon.LogPath(name)})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	writeLine := func(line logstream.Line) error {
		status := statusByName[line.Source]
		labels := cloneLabels(store.Labels[line.Source])
		if opts.Format == "raw" {
			if _, err := io.WriteString(os.Stdout, line.Text); err != nil {
				return err
			}
			if line.Terminated {
				_, err := io.WriteString(os.Stdout, "\n")
				return err
			}
			return nil
		}
		var encoded strings.Builder
		if err := logcodec.EncodeRecord(&encoded, []byte(line.Text), logcodec.RecordContext{
			Angl: line.Source, Stream: logcodec.Combined, Charge: status.Charge,
			Command: formatCommand(status.Command, status.Args), PID: status.PID,
			Sequence: line.Sequence, Path: line.Path, Truncated: line.Truncated,
			Attributes:         metadataAttributes(labels),
			ResourceAttributes: map[string]any{"service.name": line.Source, "service.namespace": "angl"},
		}, logcodec.Options{ParseJSON: true}); err != nil {
			return err
		}
		if opts.Format == "jsonl" {
			_, err := io.WriteString(os.Stdout, encoded.String())
			return err
		}
		record, err := logcodec.ParseRecord([]byte(strings.TrimSpace(encoded.String())))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, logcodec.Render(record, logcodec.RenderOptions{
			Color:    !opts.NoColor && stdoutIsTerminal() && os.Getenv("NO_COLOR") == "",
			Location: time.Local,
		}))
		return err
	}

	streamOpts := logstream.Options{TailLines: opts.Lines}
	if !opts.Follow {
		lines, err := logstream.ReadLast(ctx, sources, opts.Lines, streamOpts)
		if err != nil {
			return err
		}
		for _, line := range lines {
			if err := writeLine(line); err != nil {
				return err
			}
		}
		return nil
	}

	tailer, err := logstream.New(sources, streamOpts)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "following %d angl log(s); ctrl+c to stop\n", len(sources))
	for event := range tailer.Stream(ctx) {
		if event.Err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", event.Err)
			continue
		}
		if err := writeLine(event.Line); err != nil {
			if errors.Is(err, syscall.EPIPE) {
				return nil
			}
			return err
		}
	}
	return nil
}

func observationStatuses() ([]daemon.ProcessStatus, error) {
	result, err := rpcCallRaw("list", nil)
	if err != nil {
		return nil, err
	}
	var statuses []daemon.ProcessStatus
	if err := json.Unmarshal(result, &statuses); err != nil {
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	return statuses, nil
}

func resolveObservationNames(explicit []string, selector, view string, statuses []daemon.ProcessStatus, store catalog.Store) ([]string, error) {
	available := make(map[string]daemon.ProcessStatus, len(statuses))
	items := make([]catalog.SelectorItem, 0, len(statuses))
	for _, status := range statuses {
		available[status.Name] = status
		kind := "persistent"
		if status.Interval != "" {
			kind = "heartbeat"
		}
		items = append(items, catalog.SelectorItem{
			Name: status.Name, State: string(status.State), Enabled: status.Enabled,
			Kind: kind, Labels: store.Labels[status.Name],
		})
	}

	selected := make(map[string]bool)
	if len(explicit) == 0 {
		for name := range available {
			selected[name] = true
		}
	} else {
		for _, name := range explicit {
			if _, ok := available[name]; !ok {
				return nil, fmt.Errorf("unknown angl %q", name)
			}
			selected[name] = true
		}
	}

	combined := selector
	if view != "" {
		viewSelector, ok := store.Views[view]
		if !ok {
			return nil, fmt.Errorf("unknown view %q", view)
		}
		if combined == "" {
			combined = viewSelector
		} else if viewSelector != "" {
			combined = viewSelector + "," + combined
		}
	}
	if combined != "" {
		parsed, err := catalog.ParseSelector(combined)
		if err != nil {
			return nil, err
		}
		matched := make(map[string]bool)
		for _, item := range catalog.Resolve(parsed, items) {
			matched[item.Name] = true
		}
		for name := range selected {
			if !matched[name] {
				delete(selected, name)
			}
		}
	}

	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func cmdLabel(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: angl label <set|unset|list> ...")
	}
	switch args[0] {
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: angl label set <name> key=value [key=value...]")
		}
		name := args[1]
		labels := make(map[string]string, len(args)-2)
		for _, assignment := range args[2:] {
			key, value, ok := strings.Cut(assignment, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return fmt.Errorf("invalid label assignment %q", assignment)
			}
			labels[strings.TrimSpace(key)] = value
		}
		if _, err := rpcCallRaw("status", nameP(name)); err != nil {
			return err
		}
		if err := catalog.Update(catalog.DefaultPath(), func(store *catalog.Store) error {
			return store.Annotate(name, labels)
		}); err != nil {
			return err
		}
		fmt.Printf("labelled %s\n", name)
	case "unset":
		if len(args) < 3 {
			return fmt.Errorf("usage: angl label unset <name> key [key...]")
		}
		name := args[1]
		if _, err := rpcCallRaw("status", nameP(name)); err != nil {
			return err
		}
		if err := catalog.Update(catalog.DefaultPath(), func(store *catalog.Store) error {
			return store.RemoveLabels(name, args[2:]...)
		}); err != nil {
			return err
		}
		fmt.Printf("updated labels for %s\n", name)
	case "list":
		if len(args) != 2 {
			return fmt.Errorf("usage: angl label list <name>")
		}
		if _, err := rpcCallRaw("status", nameP(args[1])); err != nil {
			return err
		}
		store, err := catalog.Load(catalog.DefaultPath())
		if err != nil {
			return err
		}
		fmt.Println(formatLabels(store.Labels[args[1]]))
	default:
		return fmt.Errorf("unknown label command %q", args[0])
	}
	return nil
}

func cmdQuery(args []string) error {
	selector := ""
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-l", "--selector":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", args[i])
			}
			i++
			selector = args[i]
		case "--json":
			asJSON = true
		default:
			return fmt.Errorf("unknown query flag %q", args[i])
		}
	}
	statuses, err := observationStatuses()
	if err != nil {
		return err
	}
	store, err := catalog.Load(catalog.DefaultPath())
	if err != nil {
		return err
	}
	parsed, err := catalog.ParseSelector(selector)
	if err != nil {
		return err
	}
	items := make([]catalog.SelectorItem, 0, len(statuses))
	for _, status := range statuses {
		kind := "persistent"
		if status.Interval != "" {
			kind = "heartbeat"
		}
		items = append(items, catalog.SelectorItem{
			Name: status.Name, State: string(status.State), Enabled: status.Enabled,
			Kind: kind, Labels: store.Labels[status.Name],
		})
	}
	matches := catalog.Resolve(parsed, items)
	if asJSON {
		data, _ := json.MarshalIndent(matches, "", "  ")
		fmt.Println(string(data))
		return nil
	}
	for _, match := range matches {
		fmt.Println(match.Name)
	}
	return nil
}

func cmdView(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: angl view <save|list|show|delete> ...")
	}
	path := catalog.DefaultPath()
	switch args[0] {
	case "save":
		force := false
		if len(args) == 5 && args[4] == "--force" {
			force = true
		} else if len(args) != 4 {
			return fmt.Errorf("usage: angl view save <name> --selector <expr> [--force]")
		}
		if args[2] != "--selector" {
			return fmt.Errorf("usage: angl view save <name> --selector <expr> [--force]")
		}
		if err := catalog.Update(path, func(store *catalog.Store) error {
			if _, exists := store.Views[args[1]]; exists && !force {
				return fmt.Errorf("view %q already exists (use --force to replace it)", args[1])
			}
			return store.SaveView(args[1], args[3])
		}); err != nil {
			return err
		}
		fmt.Printf("saved view %s\n", args[1])
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: angl view delete <name>")
		}
		if err := catalog.Update(path, func(store *catalog.Store) error { return store.DeleteView(args[1]) }); err != nil {
			return err
		}
		fmt.Printf("deleted view %s\n", args[1])
	case "list":
		store, err := catalog.Load(path)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(store.Views))
		for name := range store.Views {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Printf("%s\t%s\n", name, store.Views[name])
		}
	case "show":
		asJSON := len(args) == 3 && args[2] == "--json"
		if len(args) != 2 && !asJSON {
			return fmt.Errorf("usage: angl view show <name> [--json]")
		}
		store, err := catalog.Load(path)
		if err != nil {
			return err
		}
		selector, ok := store.Views[args[1]]
		if !ok {
			return fmt.Errorf("unknown view %q", args[1])
		}
		if asJSON {
			data, _ := json.MarshalIndent(map[string]string{"name": args[1], "selector": selector}, "", "  ")
			fmt.Println(string(data))
		} else {
			fmt.Println(selector)
		}
	default:
		return fmt.Errorf("unknown view command %q", args[0])
	}
	return nil
}

func metadataAttributes(labels map[string]string) map[string]any {
	attrs := make(map[string]any, len(labels))
	for key, value := range labels {
		attrs["angl.metadata."+key] = value
	}
	return attrs
}

func cloneLabels(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
