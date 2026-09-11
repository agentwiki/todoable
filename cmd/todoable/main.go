// Command todoable is the CLI composition root.
package main

import (
	"github.com/agentwiki/todoable/internal/adapters/local"
	"github.com/agentwiki/todoable/internal/domain"
	"github.com/agentwiki/todoable/internal/usecases"
	"os"
)

func main() { os.Exit(run(os.Args[1:])) }
func run(args []string) int {
	dir := local.DefaultDir()
	if len(args) >= 2 && args[0] == "--data-dir" {
		dir = args[1]
		args = args[2:]
	}
	if len(args) < 3 {
		return local.Fail(domain.Invalid("supported: task register FILE, run submit FILE, run show ID --json"))
	}
	command := args[0] + " " + args[1]
	if command != "task register" && command != "run submit" && command != "run show" {
		return local.Fail(&domain.Fault{Code: 6, Kind: "not_implemented", Message: "command is not implemented"})
	}
	if (command != "run show" && len(args) != 3) || (command == "run show" && (len(args) != 4 || args[3] != "--json")) {
		return local.Fail(domain.Invalid("invalid command arguments; run show currently requires --json"))
	}
	store, err := local.Open(dir)
	if err != nil {
		return local.Fail(err)
	}
	defer func() { _ = store.Close() }()
	var value any
	switch command {
	case "task register":
		var raw []byte
		raw, err = local.ReadFile(args[2])
		if err == nil {
			var task domain.Task
			task, err = local.ParseTask(raw)
			if err == nil {
				var version int
				version, err = usecases.Register(store, task)
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
