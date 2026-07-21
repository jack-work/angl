package clix

import "testing"

func TestParseLogArgs(t *testing.T) {
	o, names, err := ParseLogArgs([]string{"api", "worker", "--output", "jsonl", "--tail", "20", "-f", "-l", "stack=apps", "-l", "state=running"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Format != "jsonl" || o.Lines != 20 || !o.Follow || len(names) != 2 || o.Selector != "stack=apps,state=running" {
		t.Fatalf("got %#v %#v", o, names)
	}
}

func TestParseLogArgsAllowsNoTarget(t *testing.T) {
	o, names, err := ParseLogArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.Follow || o.Format != "pretty" || o.Lines != 100 || len(names) != 0 {
		t.Fatalf("got %#v %#v", o, names)
	}
}

func TestParseLogArgsRejectsBad(t *testing.T) {
	for _, args := range [][]string{{"x", "--output", "xml"}, {"x", "-n", "-1"}, {"--wat"}, {"x", "--format", "jsonl"}, {"x", "--lines", "5"}} {
		if _, _, err := ParseLogArgs(args); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
}

func TestParseAnnotateArgs(t *testing.T) {
	name, o, err := ParseAnnotateArgs([]string{"api", "--set", "team=orchard", "--unset", "old"})
	if err != nil || name != "api" || o.Set["team"] != "orchard" || len(o.Unset) != 1 {
		t.Fatalf("%s %#v %v", name, o, err)
	}
}
