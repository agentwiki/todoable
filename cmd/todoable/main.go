// Command todoable is the CLI composition root.
package main

import (
	"github.com/agentwiki/todoable/internal/adapters/local"
	"github.com/agentwiki/todoable/internal/domain"
	"github.com/agentwiki/todoable/internal/usecases"
	"os"
	"strconv"
)

func main() { os.Exit(run(os.Args[1:])) }
func run(args []string) int {
	dir := local.DefaultDir()
	if len(args) >= 2 && args[0] == "--data-dir" {
		dir = args[1]
		args = args[2:]
	}
	if len(args) == 2 && args[0] == "daemon" && args[1] == "run" {
		return daemon(dir)
	}
	if len(args) < 3 {
		return local.Fail(domain.Invalid("supported: task register FILE, run submit FILE, run show ID --json"))
	}
	command := args[0] + " " + args[1]
	if command != "task register" && command != "run submit" && command != "run show" && command != "task update" && command != "task enable" && command != "task disable" {
		return local.Fail(&domain.Fault{Code: 6, Kind: "not_implemented", Message: "command is not implemented"})
	}
	if (command != "run show" && command != "task update" && len(args) != 3) || (command == "run show" && (len(args) != 4 || args[3] != "--json")) {
		return local.Fail(domain.Invalid("invalid command arguments; run show currently requires --json"))
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
	var value any
	switch command {
	case "task enable", "task disable":
		err = usecases.SetEnabled(store, args[2], command == "task enable")
		value = map[string]any{"protocol_version": 1, "task_id": args[2], "enabled": command == "task enable"}
	case "task register", "task update":
		var raw []byte
		raw, err = local.ReadFile(args[2])
		if err == nil {
			var task domain.Task
			task, err = local.ParseTask(raw)
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
		raw, err = local.ReadFile(args[2])
		if err == nil {
			value, err = usecases.Submit(store, raw)
		}
	case "run show":
		value, err = store.Run(args[2])
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
	runner := store.Executor()
	for {
		if d.Context().Err() != nil {
			return 0
		}
		worked, e := usecases.Advance(d.Context(), store, runner)
		if e != nil {
			if d.Stopped(e) {
				return 0
			}
			return local.Fail(e)
		}
		if !worked && !d.Pause() {
			return 0
		}
	}
}
