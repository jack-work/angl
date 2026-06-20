//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jack-work/schedg"
)

// cmdExec is the integrated bridge: drains the angl's message queue, then
// optionally leases from a work queue. The angl name IS the session ID.
//
// Usage: angl exec <name> [--work-queue <schedg>] [--cwd <dir>] [--runbook <path>]
func cmdExec(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: angl exec <name> [--work-queue <schedg>] [--cwd <dir>] [--runbook <path>]")
	}

	name := args[0]
	rest := args[1:]

	var workQueue, cwd, runbook string
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--work-queue":
			if i+1 < len(rest) { i++; workQueue = rest[i] }
		case "--cwd":
			if i+1 < len(rest) { i++; cwd = rest[i] }
		case "--runbook":
			if i+1 < len(rest) { i++; runbook = rest[i] }
		}
	}

	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	handled := 0

	// 1. Drain message queue.
	msgPath := execMsgQueuePath(name)
	for {
		task, db := execPopMessage(msgPath, name)
		if task == nil {
			break
		}
		handled++
		log.Printf("leased from messages: task #%s title=%q desc=%q", task.ID, task.Title, execTrunc(task.Description))
		log.Printf("in-flight: message task #%s", task.ID)
		execRunPi(name, cwd, execBuildMessagePrompt(name, task))
		db.Complete(task.ID)
		db.Save()
		db.Close()
		log.Printf("completed message task #%s", task.ID)
	}

	// 2. Check work queue (resume in-flight or lease new).
	if handled == 0 && workQueue != "" {
		task, isResume, db := execGetOrLeaseWork(workQueue, name)
		if task != nil {
			handled++
			if isResume {
				log.Printf("resuming from %s: task #%s title=%q", workQueue, task.ID, task.Title)
				execRunPi(name, cwd, execBuildResumePrompt(name, task, workQueue, runbook))
			} else {
				log.Printf("leased from %s: task #%s title=%q desc=%q", workQueue, task.ID, task.Title, execTrunc(task.Description))
				execRunPi(name, cwd, execBuildWorkPrompt(name, task, workQueue, runbook))
			}
			db.Close()
		}
	}

	if handled == 0 {
		fmt.Println("nothing to do")
	}
	return nil
}

func execMsgQueuePath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "angl", "schedg", name+".db")
}

func execPopMessage(dbPath, caller string) (*schedg.Task, *schedg.DB) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil
	}
	configName := "angl." + caller
	db, err := schedg.OpenByName(configName)
	if err != nil {
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

func execGetOrLeaseWork(queueName, caller string) (*schedg.Task, bool, *schedg.DB) {
	db, err := schedg.OpenByName(queueName)
	if err != nil {
		log.Printf("warning: %v", err)
		return nil, false, nil
	}
	for _, t := range db.Inflight() {
		m := db.Meta(t.ID)
		if m.Caller == caller {
			return &t, true, db
		}
	}
	t, ok := db.NextFor(caller)
	if !ok {
		db.Close()
		return nil, false, nil
	}
	db.Save()
	return &t, false, db
}

func execRunPi(sessionID, dir, prompt string) {
	f, err := os.CreateTemp("", "angl-prompt-*.md")
	if err != nil {
		log.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(f.Name())
	fmt.Fprint(f, prompt)
	f.Close()

	cmd := exec.Command("pi", "--session-id", sessionID, "-p", "@"+f.Name())
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("pi exited: %v", err)
	}
}

func execBuildMessagePrompt(name string, task *schedg.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are angl '%s'.\n\n", name)
	fmt.Fprintf(&b, "You have received a message:\n\n")
	if task.Description != "" {
		b.WriteString(task.Description)
	} else {
		b.WriteString(task.Title)
	}
	b.WriteString("\n")
	return b.String()
}

func execBuildResumePrompt(name string, task *schedg.Task, queue, runbook string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are angl '%s'.\n\n", name)
	fmt.Fprintf(&b, "You have task #%s in-flight from queue '%s'. Continue working on it.\n\n", task.ID, queue)
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

func execBuildWorkPrompt(name string, task *schedg.Task, queue, runbook string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are angl '%s'.\n\n", name)
	fmt.Fprintf(&b, "A task has been leased from queue '%s'.\n\n", queue)
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

func execTrunc(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
