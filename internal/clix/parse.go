package clix

import (
	"fmt"
	"strconv"
	"strings"
)

type LogOptions struct {
	Format   string
	Lines    int
	Follow   bool
	Selector string
	View     string
	NoColor  bool
}

func ParseLogArgs(args []string) (LogOptions, []string, error) {
	opts := LogOptions{Format: "pretty", Lines: 100}
	var names []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		take := func(flag string) (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", flag)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "-f", "--follow":
			opts.Follow = true
		case "--no-follow":
			opts.Follow = false
		case "--no-color":
			opts.NoColor = true
		case "-n", "--tail":
			value, err := take(arg)
			if err != nil {
				return opts, nil, err
			}
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return opts, nil, fmt.Errorf("invalid line count %q", value)
			}
			opts.Lines = n
		case "-o", "--output":
			value, err := take(arg)
			if err != nil {
				return opts, nil, err
			}
			switch value {
			case "pretty", "jsonl", "raw":
				opts.Format = value
			default:
				return opts, nil, fmt.Errorf("invalid format %q (want pretty, jsonl, or raw)", value)
			}
		case "-l", "--selector":
			value, err := take(arg)
			if err != nil {
				return opts, nil, err
			}
			if opts.Selector == "" {
				opts.Selector = value
			} else {
				opts.Selector += "," + value
			}
		case "--view":
			value, err := take(arg)
			if err != nil {
				return opts, nil, err
			}
			opts.View = value
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, nil, fmt.Errorf("unknown log flag %q", arg)
			}
			names = append(names, arg)
		}
	}
	return opts, names, nil
}

type AnnotateOptions struct {
	Set   map[string]string
	Unset []string
}

func ParseAnnotateArgs(args []string) (string, AnnotateOptions, error) {
	if len(args) == 0 {
		return "", AnnotateOptions{}, fmt.Errorf("angl name required")
	}
	name := args[0]
	opts := AnnotateOptions{Set: map[string]string{}}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--set":
			if i+1 >= len(args) {
				return "", opts, fmt.Errorf("--set requires key=value")
			}
			i++
			k, v, ok := strings.Cut(args[i], "=")
			if !ok || strings.TrimSpace(k) == "" {
				return "", opts, fmt.Errorf("invalid metadata assignment %q", args[i])
			}
			opts.Set[strings.TrimSpace(k)] = v
		case "--unset":
			if i+1 >= len(args) {
				return "", opts, fmt.Errorf("--unset requires a key")
			}
			i++
			opts.Unset = append(opts.Unset, strings.TrimSpace(args[i]))
		default:
			return "", opts, fmt.Errorf("unknown annotate flag %q", args[i])
		}
	}
	if len(opts.Set) == 0 && len(opts.Unset) == 0 {
		return "", opts, fmt.Errorf("provide --set or --unset")
	}
	return name, opts, nil
}
