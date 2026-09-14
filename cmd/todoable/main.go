// Command todoable is the CLI composition root.
package main

import (
	"github.com/agentwiki/todoable/internal/adapters/local"
	"github.com/agentwiki/todoable/internal/domain"
	"github.com/agentwiki/todoable/internal/usecases"
	"os"
	"strconv"
	"sync"
)

func main() { os.Exit(run(os.Args[1:])) }
func run(args []string) int {
	dir := local.DefaultDir()
	if len(args) >= 2 && args[0] == "--data-dir" {
		dir = args[1]
		args = args[2:]
	}
	if len(args) > 1 && args[0] == "submission" && args[1] == "cancel" {
		return cancelCommand(dir, args)
	}
	if len(args) > 0 && (args[0] == "status" || args[0] == "logs" || len(args) > 1 && (args[0] == "task" || args[0] == "run") && args[1] == "show") {
		return observeCommand(dir, args)
	}
	if len(args) > 1 && args[0] == "task" && args[1] == "cancel" {
		return taskCancelCommand(dir, args)
	}
	if len(args) > 0 && args[0] == "resume" {
		return resumeCommand(dir, args)
	}
	if len(args) > 0 && args[0] == "schedule" {
		return scheduleCommand(dir, args)
	}
	if len(args) == 1 && args[0] == "daemon" || len(args) == 2 && args[0] == "daemon" && args[1] == "run" {
		return daemon(dir)
	}
	if len(args) < 3 {
		return local.Fail(domain.Invalid("expected a supported command and its arguments"))
	}
	command := args[0] + " " + args[1]
	if command != "task register" && command != "run submit" && command != "task update" && command != "task enable" && command != "task disable" {
		return local.Fail(domain.Invalid("unknown command"))
	}
	if command != "task update" && len(args) != 3 {
		return local.Fail(domain.Invalid("invalid command arguments"))
	}
	expected := 0
	if command == "task update" {
		if len(args) != 5 || args[3] != "--if-version" {
			return local.Fail(domain.Invalid("task update requires --if-version N"))
		}
		var e error
		expected, e = strconv.Atoi(args[4])
		if e != nil || expected < 1 {
			return local.Fail(domain.Invalid("invalid expected version"))
		}
	}
	store, err := local.Open(dir)
	if err != nil {
		return local.Fail(err)
	}
	defer func() { _ = store.Close() }()
	if err = store.CheckCLIConfig(); err != nil {
		return local.Fail(err)
	}
	var value any
	switch command {
	case "task enable", "task disable":
		err = usecases.SetEnabled(store, args[2], command == "task enable")
		value = map[string]any{"protocol_version": 1, "task_id": args[2], "enabled": command == "task enable"}
	case "task register", "task update":
		var raw []byte
		raw, err = store.ReadManifest(args[2])
		if err == nil {
			var task domain.Task
			task, err = store.ParseTask(raw)
			if err == nil {
				var version int
				if command == "task update" {
					version, err = usecases.Update(store, task, expected)
				} else {
					version, err = usecases.Register(store, task)
				}
				value = map[string]any{"protocol_version": 1, "task_id": task.ID, "task_version": version}
			}
		}
	case "run submit":
		var raw []byte
		raw, err = store.ReadSubmission(args[2])
		if err == nil {
			value, err = usecases.Submit(store, raw)
		}
	}
	if err != nil {
		return local.Fail(err)
	}
	if err = local.Write(value); err != nil {
		return local.Fail(err)
	}
	return 0
}

func daemon(dir string) int {
	store, err := local.Open(dir)
	if err != nil {
		return local.Fail(err)
	}
	defer func() { _ = store.Close() }()
	d, err := store.Daemon()
	if err != nil {
		return local.Fail(err)
	}
	defer func() { _ = d.Close() }()
	if err = store.CleanupLogs(); err != nil {
		return local.Fail(err)
	}
	if err = usecases.PublishSchedules(store); err != nil {
		return local.Fail(err)
	}
	runner := store.Executor()
	var workers sync.WaitGroup
	errors := make(chan error, store.WorkerCount()+1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		for d.Context().Err() == nil {
			if d.CleanupDue() {
				if e := store.RecordDaemonState("running"); e != nil {
					errors <- e
					d.Stop()
					return
				}
				if e := store.CleanupLogs(); e != nil {
					errors <- e
					d.Stop()
					return
				}
			}
			if e := store.ReconcileCancellations(d.Context()); e != nil {
				if d.Stopped(e) {
					return
				}
				errors <- e
				d.Stop()
				return
			}
			if e := usecases.PublishSchedules(store); e != nil {
				errors <- e
				d.Stop()
				return
			}
			if !d.Pause() {
				return
			}
		}
	}()
	for range store.WorkerCount() {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for d.Context().Err() == nil {
				worked, e := usecases.Advance(d.Context(), store, runner)
				if e != nil {
					errors <- e
					d.Stop()
					return
				}
				if !worked && !d.Pause() {
					return
				}
			}
		}()
	}
	workers.Wait()
	close(errors)
	for e := range errors {
		return local.Fail(e)
	}
	return 0
}
