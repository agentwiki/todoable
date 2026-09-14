package main

import (
	"strconv"

	"github.com/agentwiki/todoable/internal/adapters/local"
	"github.com/agentwiki/todoable/internal/domain"
)

func observeCommand(dir string, args []string) int {
	command := args[0]
	id, step, task := "", "", ""
	version := 0
	asJSON, follow := false, false
	start := 1
	switch command {
	case "task", "run":
		if len(args) < 3 || args[1] != "show" {
			return local.Fail(domain.Invalid("show requires an identifier"))
		}
		id = args[2]
		start = 3
	case "logs":
		if len(args) < 2 {
			return local.Fail(domain.Invalid("logs requires RUN_ID"))
		}
		id = args[1]
		start = 2
	}
	seen := map[string]bool{}
	for i := start; i < len(args); i++ {
		option := args[i]
		if seen[option] {
			return local.Fail(domain.Invalid("duplicate option"))
		}
		seen[option] = true
		if option == "--json" && command != "logs" {
			asJSON = true
			continue
		}
		if option == "--follow" && command == "logs" {
			follow = true
			continue
		}
		allowed := option == "--version" && command == "task" || option == "--task" && command == "status" || option == "--step" && command == "logs"
		if !allowed || i+1 >= len(args) {
			return local.Fail(domain.Invalid("invalid query option"))
		}
		i++
		value := args[i]
		switch option {
		case "--version":
			var err error
			version, err = strconv.Atoi(value)
			if err != nil || version < 1 {
				return local.Fail(domain.Invalid("invalid Task version"))
			}
		case "--task":
			if !domain.ValidID(value) {
				return local.Fail(domain.Invalid("invalid Task ID"))
			}
			task = value
		case "--step":
			if value == "" {
				return local.Fail(domain.Invalid("empty Step ID"))
			}
			step = value
		}
	}
	store, err := local.Open(dir)
	if err != nil {
		return local.Fail(err)
	}
	defer store.Close()
	var value map[string]any
	switch command {
	case "task":
		value, err = store.TaskView(id, version)
	case "run":
		value, err = store.Run(id)
	case "status":
		value, err = store.Status(task)
	case "logs":
		err = store.WriteLogs(id, step, follow)
	}
	if err != nil {
		return local.Fail(err)
	}
	if command == "logs" {
		return 0
	}
	if asJSON {
		err = local.Write(value)
	} else {
		err = local.WriteObservation(command, value)
	}
	if err != nil {
		return local.Fail(err)
	}
	return 0
}
