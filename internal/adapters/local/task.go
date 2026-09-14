package local

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
	"gopkg.in/yaml.v3"
)

func ParseTask(raw []byte) (domain.Task, error) {
	return parseTask(raw, DefaultConfig())
}
func parseTask(raw []byte, config Config) (domain.Task, error) {
	var task domain.Task
	raw, e := manifestJSON(raw, config.MaxManifestBytes)
	if e != nil {
		return task, e
	}
	task.Finish.MaxCalls = 3
	task.RepeatDelay = "0s"
	task.Env = map[string]string{}
	task.InheritEnv = []string{}
	task.Limits = map[string]string{}
	if e := domain.Decode(raw, &task); e != nil {
		return task, e
	}
	if task.Version != 1 || !domain.ValidID(task.ID) || !filepath.IsAbs(task.Workdir) || strings.TrimSpace(task.Prompt) == "" || task.Repeat < 0 || task.Repeat > config.MaxRepeat || task.Finish.MaxCalls < 1 || task.Finish.MaxCalls > config.MaxCallsPerRun {
		return task, domain.Invalid("invalid Task fields")
	}
	info, e := os.Stat(task.Workdir)
	if e != nil || !info.IsDir() {
		return task, domain.Invalid("workdir must be an existing absolute directory")
	}
	task.Workdir, e = filepath.EvalSymlinks(task.Workdir)
	if e != nil {
		return task, e
	}
	commands := [][]string{task.Agent, task.Finish.Check}
	if task.Before != nil {
		commands = append(commands, task.Before)
	}
	if task.After != nil {
		commands = append(commands, task.After)
	}
	if task.Start != nil {
		commands = append(commands, task.Start.Check)
		if task.Start.PollEvery == "" {
			task.Start.PollEvery = "60s"
		}
		if task.Start.WaitTimeout == "" {
			task.Start.WaitTimeout = "0s"
		}
		if !duration(task.Start.PollEvery, time.Second, capDuration(config, "start_poll_every")) || !duration(task.Start.WaitTimeout, 0, capDuration(config, "start_wait_timeout")) {
			return task, domain.Invalid("invalid start durations")
		}
	}
	for _, c := range commands {
		if len(c) == 0 || c[0] == "" {
			return task, domain.Invalid("command must contain executable")
		}
		for _, a := range c {
			if strings.ContainsRune(a, 0) {
				return task, domain.Invalid("NUL in command")
			}
		}
	}
	if !duration(task.RepeatDelay, 0, capDuration(config, "repeat_delay")) {
		return task, domain.Invalid("invalid repeat_delay")
	}
	defaults := map[string]string{"start_check_timeout": "30s", "finish_check_timeout": "30s", "before_timeout": "5m", "agent_timeout": "30m", "after_timeout": "5m", "run_timeout": "2h"}
	caps := map[string]time.Duration{"start_check_timeout": 5 * time.Minute, "finish_check_timeout": 5 * time.Minute, "before_timeout": time.Hour, "agent_timeout": 4 * time.Hour, "after_timeout": time.Hour, "run_timeout": 24 * time.Hour}
	for k, v := range task.Limits {
		_, ok := caps[k]
		cap := capDuration(config, k)
		if !ok || !duration(v, time.Nanosecond, cap) {
			return task, domain.Invalid("invalid limit: " + k)
		}
	}
	if task.Limits == nil {
		task.Limits = map[string]string{}
	}
	for k, v := range defaults {
		if _, ok := task.Limits[k]; !ok {
			task.Limits[k] = v
		}
		if !duration(task.Limits[k], time.Nanosecond, capDuration(config, k)) {
			return task, domain.Invalid("limit exceeds installation cap: " + k)
		}
	}
	for k, v := range task.Env {
		if !envName(k) || strings.ContainsRune(v, 0) {
			return task, domain.Invalid("invalid environment")
		}
	}
	for _, k := range task.InheritEnv {
		if !envName(k) {
			return task, domain.Invalid("invalid inherited environment name")
		}
	}
	if task.Schedule != nil {
		if task.Schedule.Every != "" && !duration(task.Schedule.Every, time.Second, 8760*time.Hour) {
			return task, domain.Invalid("invalid schedule every")
		}
		envelope, _ := json.Marshal(struct {
			Data       json.RawMessage   `json:"data"`
			Occurrence domain.Occurrence `json:"occurrence"`
		}{task.Schedule.Input, domain.Occurrence{ScheduledAt: "9999-12-31T23:59:59.999999999Z", WindowStart: "9999-12-31T23:59:59.999999999Z", WindowEnd: "9999-12-31T23:59:59.999999999Z"}})
		if len(envelope) > config.MaxInputBytes {
			return task, domain.Invalid("schedule input too large")
		}

		if task.Schedule.Cron != "" && task.Schedule.Timezone == "" {
			task.Schedule.Timezone = "UTC"
		}
		if _, e := compileSchedule(*task.Schedule, time.Now()); e != nil {
			return task, e
		}
	}
	return task, nil
}
func envName(k string) bool {
	return k != "" && !strings.ContainsAny(k, "=\x00") && !strings.HasPrefix(k, "TODOABLE_")
}

var durationPattern = regexp.MustCompile(`^(?:(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:ns|us|ms|s|m|h))+$`)

func duration(s string, min, max time.Duration) bool {
	if !durationPattern.MatchString(s) {
		return false
	}
	d, e := time.ParseDuration(s)
	return e == nil && d >= min && d <= max
}
func capDuration(config Config, key string) time.Duration {
	d, _ := time.ParseDuration(config.Caps[key])
	return d
}
func manifestJSON(raw []byte, limit int) ([]byte, error) {
	if len(raw) > limit {
		return nil, domain.Invalid("manifest too large")
	}
	if !json.Valid(raw) {
		var node yaml.Node
		d := yaml.NewDecoder(bytes.NewReader(raw))
		if e := d.Decode(&node); e != nil {
			return nil, domain.Invalid(e.Error())
		}
		var next yaml.Node
		if d.Decode(&next) != io.EOF {
			return nil, domain.Invalid("expected one YAML document")
		}
		budget := limit
		value, e := yamlValue(&node, 0, &budget, map[*yaml.Node]bool{})
		if e != nil {
			return nil, e
		}
		raw, e = json.Marshal(value)
		if e != nil {
			return nil, domain.Invalid(e.Error())
		}
	}
	normalized, e := domain.Canonical(raw)
	if e != nil {
		return nil, e
	}
	if len(raw) > limit || len(normalized) > limit {
		return nil, domain.Invalid("expanded manifest too large")
	}
	if len(normalized) == 0 || normalized[0] != '{' {
		return nil, domain.Invalid("manifest must be an object")
	}
	return normalized, nil
}
func yamlValue(n *yaml.Node, depth int, budget *int, active map[*yaml.Node]bool) (any, error) {
	if depth > 64 || *budget < 0 {
		return nil, domain.Invalid("YAML expansion exceeds size or depth limit")
	}
	if active[n] {
		return nil, domain.Invalid("cyclic YAML alias")
	}
	if n.Style&yaml.TaggedStyle != 0 || n.Tag == "!!merge" {
		return nil, domain.Invalid("YAML tags and merge keys are not supported")
	}
	active[n] = true
	defer delete(active, n)
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, domain.Invalid("empty YAML")
		}
		return yamlValue(n.Content[0], depth, budget, active)
	case yaml.AliasNode:
		return yamlValue(n.Alias, depth, budget, active)
	case yaml.MappingNode:
		if depth >= 64 {
			return nil, domain.Invalid("YAML too deep")
		}
		*budget -= 2
		m := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			key, e := yamlValue(n.Content[i], depth+1, budget, active)
			if e != nil {
				return nil, e
			}
			k, ok := key.(string)
			if !ok {
				return nil, domain.Invalid("YAML object key must be string")
			}
			if _, ok := m[k]; ok {
				return nil, domain.Invalid("duplicate YAML key")
			}
			*budget--
			if i > 0 {
				*budget--
			}
			v, e := yamlValue(n.Content[i+1], depth+1, budget, active)
			if e != nil {
				return nil, e
			}
			m[k] = v
		}
		return m, nil
	case yaml.SequenceNode:
		if depth >= 64 {
			return nil, domain.Invalid("YAML too deep")
		}
		*budget -= 2
		a := []any{}
		for i, c := range n.Content {
			if i > 0 {
				*budget--
			}
			v, e := yamlValue(c, depth+1, budget, active)
			if e != nil {
				return nil, e
			}
			a = append(a, v)
		}
		return a, nil
	case yaml.ScalarNode:
		var v any
		switch n.Tag {
		case "!!str":
			v = n.Value
		case "!!null":
			v = nil
		case "!!bool", "!!int", "!!float":
			if e := n.Decode(&v); e != nil {
				return nil, domain.Invalid(e.Error())
			}
		default:
			return nil, domain.Invalid(fmt.Sprintf("unsupported YAML value %s", n.Tag))
		}
		encoded, e := json.Marshal(v)
		if e != nil {
			return nil, domain.Invalid(e.Error())
		}
		*budget -= len(encoded)
		if *budget < 0 {
			return nil, domain.Invalid("expanded manifest too large")
		}
		return v, nil
	}
	return nil, domain.Invalid("unsupported YAML node")
}
func snapshot(task domain.Task) ([]byte, error) {
	env := map[string]string{}
	for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for _, k := range task.InheritEnv {
		v, ok := os.LookupEnv(k)
		if !ok {
			return nil, domain.Invalid("missing inherited environment: " + k)
		}
		env[k] = v
	}
	for k, v := range task.Env {
		env[k] = v
	}
	task.Env = env
	return json.Marshal(task)
}
