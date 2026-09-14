package main

import (
	"strconv"

	"github.com/agentwiki/todoable/internal/adapters/local"
	"github.com/agentwiki/todoable/internal/domain"
	"github.com/agentwiki/todoable/internal/usecases"
)

func resumeCommand(dir string, args []string) int {
	if len(args) < 2 {
		return local.Fail(domain.Invalid("resume requires RUN_ID"))
	}
	r := domain.ResumeRequest{RunID: args[1]}
	seen := map[string]bool{}
	for i := 2; i < len(args); i++ {
		option := args[i]
		if seen[option] {
			return local.Fail(domain.Invalid("duplicate option"))
		}
		seen[option] = true
		if option == "--processes-stopped" {
			r.ProcessesStopped = true
			continue
		}
		if i+1 >= len(args) {
			return local.Fail(domain.Invalid("missing option value"))
		}
		i++
		value := args[i]
		switch option {
		case "--step":
			r.StepID = value
		case "--action":
			r.Action = value
		case "--reason":
			r.Reason = value
		case "--exit-code":
			n, err := strconv.Atoi(value)
			if err != nil {
				return local.Fail(domain.Invalid("invalid exit code"))
			}
			r.ExitCode = &n
		default:
			return local.Fail(domain.Invalid("unknown resume option"))
		}
	}
	store, err := local.Open(dir)
	if err != nil {
		return local.Fail(err)
	}
	defer store.Close()
	if err = store.CheckCLIConfig(); err != nil {
		return local.Fail(err)
	}
	result, err := usecases.Resume(store, r)
	if err != nil {
		return local.Fail(err)
	}
	if err = local.Write(result); err != nil {
		return local.Fail(err)
	}
	return 0
}

func cancelCommand(dir string, args []string) int {
	if len(args) < 3 {
		return local.Fail(domain.Invalid("submission cancel requires ID"))
	}
	r := domain.CancelRequest{SubmissionID: args[2]}
	seen := map[string]bool{}
	for i := 3; i < len(args); i++ {
		option := args[i]
		if seen[option] {
			return local.Fail(domain.Invalid("duplicate option"))
		}
		seen[option] = true
		switch option {
		case "--acknowledge-effects":
			r.AcknowledgeEffects = true
		case "--processes-stopped":
			r.ProcessesStopped = true
		case "--reason":
			if i+1 >= len(args) {
				return local.Fail(domain.Invalid("missing cancellation reason"))
			}
			i++
			r.Reason = args[i]
		default:
			return local.Fail(domain.Invalid("unknown cancellation option"))
		}
	}
	store, err := local.Open(dir)
	if err != nil {
		return local.Fail(err)
	}
	defer store.Close()
	if err = store.CheckCLIConfig(); err != nil {
		return local.Fail(err)
	}
	result, err := usecases.Cancel(store, r)
	if err != nil {
		return local.Fail(err)
	}
	if err = local.Write(result); err != nil {
		return local.Fail(err)
	}
	return 0
}
