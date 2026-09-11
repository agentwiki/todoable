package main

import (
	"github.com/agentwiki/todoable/internal/adapters/local"
	"github.com/agentwiki/todoable/internal/domain"
	"github.com/agentwiki/todoable/internal/usecases"
	"strconv"
)

func scheduleCommand(dir string, args []string) int {
	if len(args) < 3 {
		return local.Fail(domain.Invalid("schedule requires command and Task ID"))
	}
	var value any
	var err error
	command := args[1]
	switch command {
	case "enable", "disable":
		if len(args) != 3 {
			return local.Fail(domain.Invalid("unexpected schedule arguments"))
		}
	case "show":
		if len(args) != 3 && (len(args) != 4 || args[3] != "--json") {
			return local.Fail(domain.Invalid("schedule show ID [--json]"))
		}
	case "submit":
		if (len(args) != 5 && len(args) != 7) || args[3] != "--at" {
			return local.Fail(domain.Invalid("schedule submit ID --at TIME [--task-version N]"))
		}
	default:
		return local.Fail(domain.Invalid("unknown schedule command"))
	}
	store, err := local.Open(dir)
	if err != nil {
		return local.Fail(err)
	}
	defer func() { _ = store.Close() }()
	switch command {
	case "enable", "disable":
		err = usecases.SetSchedule(store, args[2], command == "enable")
		if err == nil {
			value, err = usecases.Schedule(store, args[2])
		}
	case "show":
		value, err = usecases.Schedule(store, args[2])
	case "submit":
		at, e := domain.ParseScheduledAt(args[4])
		if e != nil {
			return local.Fail(e)
		}
		version := 0
		if len(args) == 7 {
			if args[5] != "--task-version" {
				return local.Fail(domain.Invalid("unexpected schedule option"))
			}
			version, e = strconv.Atoi(args[6])
			if e != nil || version < 1 {
				return local.Fail(domain.Invalid("invalid Task version"))
			}
		}
		value, err = usecases.SubmitSchedule(store, args[2], at, version)
	}
	if err != nil {
		return local.Fail(err)
	}
	if command == "show" && len(args) == 3 {
		err = local.WriteSchedule(value.(map[string]any))
	} else {
		err = local.Write(value)
	}
	if err != nil {
		return local.Fail(err)
	}
	return 0
}
