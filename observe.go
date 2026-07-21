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
			_, err := fmt.Fprintln(os.Stdout, line.Text)
			return err
		}
		var encoded strings.Builder
		adapter := logcodec.NewAdapter(&encoded, logcodec.Options{
			Metadata: logcodec.Metadata{
				Angl: line.Source, Stream: logcodec.Stdout, Charge: status.Charge,
				PID: status.PID, Attributes: metadataAttributes(labels),
				ResourceAttributes: map[string]any{"service.name": line.Source, "service.namespace": "angl"},
			},
			ParseJSON: true,
		})
		if _, err := adapter.Write([]byte(line.Text + "\n")); err != nil {
			return err
		}
		if err := adapter.Close(); err != nil {
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
	selected := make(map[string]bool)
	for _, name := range explicit {
		selected[name] = true
	}
	if view != "" {
		viewSelector, ok := store.Views[view]
		if !ok {
			return nil, fmt.Errorf("unknown view %q", view)
		}
		if selector == "" {
			selector = viewSelector
		} else if viewSelector != "" {
			selector = viewSelector + "," + selector
		}
	}
	if selector != "" {
		parsed, err := catalog.ParseSelector(selector)
		if err != nil {
			return nil, err
		}
		for _, status := range statuses {
			if parsed.Matches(store.Labels[status.Name]) {
				selected[status.Name] = true
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

func cmdAnnotate(args []string) error {
	name, opts, err := clix.ParseAnnotateArgs(args)
	if err != nil {
		return err
	}
	if _, err := rpcCallRaw("status", nameP(name)); err != nil {
		return err
	}
	if err := catalog.Update(catalog.DefaultPath(), func(store *catalog.Store) error {
		if len(opts.Set) > 0 {
			if err := store.Annotate(name, opts.Set); err != nil {
				return err
			}
		}
		return store.RemoveLabels(name, opts.Unset...)
	}); err != nil {
		return err
	}
	fmt.Printf("annotated %s\n", name)
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
	matches := make([]map[string]any, 0)
	for _, status := range statuses {
		if parsed.Matches(store.Labels[status.Name]) {
			matches = append(matches, map[string]any{"name": status.Name, "state": status.State, "enabled": status.Enabled, "metadata": cloneLabels(store.Labels[status.Name])})
		}
	}
	if asJSON {
		data, _ := json.MarshalIndent(matches, "", "  ")
		fmt.Println(string(data))
		return nil
	}
	for _, match := range matches {
		fmt.Println(match["name"])
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
		if len(args) != 4 || args[2] != "--selector" {
			return fmt.Errorf("usage: angl view save <name> --selector <expr>")
		}
		if err := catalog.Update(path, func(store *catalog.Store) error { return store.SaveView(args[1], args[3]) }); err != nil {
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
		if len(args) != 2 {
			return fmt.Errorf("usage: angl view show <name>")
		}
		store, err := catalog.Load(path)
		if err != nil {
			return err
		}
		selector, ok := store.Views[args[1]]
		if !ok {
			return fmt.Errorf("unknown view %q", args[1])
		}
		fmt.Println(selector)
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
