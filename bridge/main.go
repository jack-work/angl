// angl-bridge: stateless CLI that drains an angl's message queue, then
// optionally leases one task from a work queue, passing each to pi.
//
// Usage:
//   angl-bridge --session <id> [--work-queue <schedg-name>] [--cwd <dir>] [--runbook <path>]
//
// Message queue: ~/.config/angl/schedg/<session>.db (SQLite, created by daemon).
// Messages are drained completely (loop until empty) before checking the work queue.
// Work queue: resolved by name from schedg config. One task per invocation.
//
// If both queues are empty, prints "nothing to do" and exits 0.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jack-work/schedg"
)

func main() {
	sessionID := flag.String("session", "", "pi session ID (= angl name)")
	workQueue := flag.String("work-queue", "", "named schedg to drain (resolved via schedg config)")
	cwd := flag.String("cwd", "", "working directory for pi")
	runbook := flag.String("runbook", "", "runbook path to include in work-queue prompts")
	flag.Parse()

	if *sessionID == "" {
		fmt.Fprintln(os.Stderr, "usage: angl-bridge --session <id> [--work-queue <name>] [--cwd <dir>] [--runbook <path>]")
		os.Exit(2)
	}

	dir := *cwd
	if dir == "" {
		dir, _ = os.Getwd()
	}

	handled := 0

	// 1. Drain the message queue completely.
	msgPath := messageQueuePath(*sessionID)
	for {
		task, db := popMessage(msgPath, *sessionID)
		if task == nil {
			break
		}
		handled++
		log.Printf("leased from messages: task #%s title=%q desc=%q", task.ID, task.Title, truncDesc(task.Description))
		log.Printf("in-flight: message task #%s", task.ID)
		runPi(*sessionID, dir, buildMessagePrompt(*sessionID, task))
		db.Complete(task.ID)
		db.Save()
		db.Close()
		log.Printf("completed message task #%s", task.ID)
	}

	// 2. If no messages were handled, check the work queue.
	//    First check for an in-flight task (resume), then lease a new one.
	if handled == 0 && *workQueue != "" {
		task, isResume, db := getOrLeaseWork(*workQueue, *sessionID)
		if task != nil {
			handled++
			if isResume {
				log.Printf("resuming from %s: task #%s title=%q", *workQueue, task.ID, task.Title)
				runPi(*sessionID, dir, buildResumePrompt(*sessionID, task, *workQueue, *runbook))
			} else {
				log.Printf("leased from %s: task #%s title=%q desc=%q", *workQueue, task.ID, task.Title, truncDesc(task.Description))
				runPi(*sessionID, dir, buildWorkPrompt(*sessionID, task, *workQueue, *runbook))
			}
			db.Close()
		}
	}

	if handled == 0 {
		fmt.Println("nothing to do")
	}
}

// ---------------------------------------------------------------------------

func messageQueuePath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "angl", "schedg", name+".db")
}

func popMessage(dbPath, caller string) (*schedg.Task, *schedg.DB) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil
	}
	// Open by name so the state file matches what the schedg-web uses.
	name := "angl." + caller
	db, err := schedg.OpenByName(name)
	if err != nil {
		// Fallback to direct open if not registered in config yet.
		db, err = schedg.Open(schedg.Options{Driver: "sqlite", Path: dbPath})
	}
	if err != nil {
		log.Printf("warning: open message queue: %v", err)
		return nil, nil
	}
	t, ok := db.NextFor(caller)
	if !ok {
		db.Close()
		return nil, nil
	}
	db.Save()
	return &t, db
}

// getOrLeaseWork checks for an in-flight task first (resume from previous tick),
// then leases a new one if nothing is in-flight.
func getOrLeaseWork(name, caller string) (*schedg.Task, bool, *schedg.DB) {
	db, err := schedg.OpenByName(name)
	if err != nil {
		log.Printf("warning: %v", err)
		return nil, false, nil
	}

	// Check for in-flight tasks owned by this caller.
	inflight := db.Inflight()
	for _, t := range inflight {
		meta := db.Meta(t.ID)
		if meta.Caller == caller {
			return &t, true, db
		}
	}

	// Nothing in-flight for us, lease a new one.
	t, ok := db.NextFor(caller)
	if !ok {
		db.Close()
		return nil, false, nil
	}
	db.Save()
	return &t, false, db
}

func popWorkQueue(name, caller string) (*schedg.Task, *schedg.DB) {
	db, err := schedg.OpenByName(name)
	if err != nil {
		log.Printf("warning: %v", err)
		return nil, nil
	}
	t, ok := db.NextFor(caller)
	if !ok {
		db.Close()
		return nil, nil
	}
	db.Save()
	return &t, db
}

func runPi(sessionID, dir, prompt string) {
	promptFile, err := os.CreateTemp("", "angl-prompt-*.md")
	if err != nil {
		log.Fatalf("create temp file: %v", err)
	}
	promptPath := promptFile.Name()
	defer os.Remove(promptPath)
	fmt.Fprint(promptFile, prompt)
	promptFile.Close()

	cmd := exec.Command("pi", "--session-id", sessionID, "-p", "@"+promptPath)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Printf("pi exited: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Prompt builders
// ---------------------------------------------------------------------------

func buildMessagePrompt(anglName string, task *schedg.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are angl '%s'.\n\n", anglName)
	fmt.Fprintf(&b, "You have received a message:\n\n")
	if task.Description != "" {
		b.WriteString(task.Description)
	} else {
		b.WriteString(task.Title)
	}
	b.WriteString("\n")
	return b.String()
}

func buildResumePrompt(anglName string, task *schedg.Task, queueName, runbook string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are angl '%s'.\n\n", anglName)
	fmt.Fprintf(&b, "You have task #%s in-flight from queue '%s'. Continue working on it.\n\n", task.ID, queueName)
	if task.Title != "" {
		fmt.Fprintf(&b, "**Title**: %s\n", task.Title)
	}
	fmt.Fprintf(&b, "**Priority**: %d\n\n", task.Priority)
	if task.Description != "" {
		fmt.Fprintf(&b, "### Description\n\n%s\n\n", task.Description)
	}
	if runbook != "" {
		fmt.Fprintf(&b, "Read and follow the runbook at: %s\n\n", runbook)
	}
	return b.String()
}

func buildWorkPrompt(anglName string, task *schedg.Task, queueName, runbook string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are angl '%s'.\n\n", anglName)
	fmt.Fprintf(&b, "A task has been leased from queue '%s'.\n\n", queueName)
	fmt.Fprintf(&b, "## Task #%s\n\n", task.ID)
	if task.Title != "" {
		fmt.Fprintf(&b, "**Title**: %s\n", task.Title)
	}
	fmt.Fprintf(&b, "**Priority**: %d\n\n", task.Priority)
	if task.Description != "" {
		fmt.Fprintf(&b, "### Description\n\n%s\n\n", task.Description)
	}
	if runbook != "" {
		fmt.Fprintf(&b, "Read and follow the runbook at: %s\n\n", runbook)
	}
	return b.String()
}

func truncDesc(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
