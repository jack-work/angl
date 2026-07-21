package clix

import "testing"

func TestParseLogArgs(t *testing.T) {
	o, names, err := ParseLogArgs([]string{"api", "worker", "--format", "jsonl", "-n", "20", "-f", "--since", "5m"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Format != "jsonl" || o.Lines != 20 || !o.Follow || o.Since.String() != "5m0s" || len(names) != 2 {
		t.Fatalf("got %#v %#v", o, names)
	}
}
func TestParseLogArgsRejectsBad(t *testing.T) {
	for _, args := range [][]string{{}, {"x", "--format", "xml"}, {"x", "-n", "-1"}, {"--wat"}} {
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
