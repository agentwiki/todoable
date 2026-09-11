package local

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
	"gopkg.in/yaml.v3"
)

func ParseTask(raw []byte) (domain.Task, error) {
	var task domain.Task
	if len(raw) > 1048576 {
		return task, domain.Invalid("manifest too large")
	}
	if !json.Valid(raw) {
		var node yaml.Node
		d := yaml.NewDecoder(bytes.NewReader(raw))
		if e := d.Decode(&node); e != nil {
			return task, domain.Invalid(e.Error())
		}
		var next yaml.Node
		if d.Decode(&next) != io.EOF {
			return task, domain.Invalid("expected one YAML document")
		}
		value, e := yamlValue(&node, 0)
		if e != nil {
			return task, e
		}
		raw, e = json.Marshal(value)
		if e != nil {
			return task, domain.Invalid(e.Error())
		}
	}
	if len(raw) > 1048576 {
		return task, domain.Invalid("expanded manifest too large")
	}
	task.Finish.MaxCalls = 3
	task.RepeatDelay = "0s"
	task.Env = map[string]string{}
	task.InheritEnv = []string{}
	task.Limits = map[string]string{}
	if e := domain.Decode(raw, &task); e != nil {
		return task, e
	}
	if task.Version != 1 || !domain.ValidID(task.ID) || !filepath.IsAbs(task.Workdir) || strings.TrimSpace(task.Prompt) == "" || task.Repeat < 0 || task.Repeat > 1000 || task.Finish.MaxCalls < 1 || task.Finish.MaxCalls > 100 {
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
		if !duration(task.Start.PollEvery, time.Second, 24*time.Hour) || !duration(task.Start.WaitTimeout, 0, 720*time.Hour) {
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
	if !duration(task.RepeatDelay, 0, 24*time.Hour) {
		return task, domain.Invalid("invalid repeat_delay")
	}
	defaults := map[string]string{"start_check_timeout": "30s", "finish_check_timeout": "30s", "before_timeout": "5m", "agent_timeout": "30m", "after_timeout": "5m", "run_timeout": "2h"}
	caps := map[string]time.Duration{"start_check_timeout": 5 * time.Minute, "finish_check_timeout": 5 * time.Minute, "before_timeout": time.Hour, "agent_timeout": 4 * time.Hour, "after_timeout": time.Hour, "run_timeout": 24 * time.Hour}
	for k, v := range task.Limits {
		cap, ok := caps[k]
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
	return task, nil
}
func envName(k string) bool {
	return k != "" && !strings.ContainsAny(k, "=\x00") && !strings.HasPrefix(k, "TODOABLE_")
}
func duration(s string, min, max time.Duration) bool {
	d, e := time.ParseDuration(s)
	return e == nil && d >= min && d <= max
}
func yamlValue(n *yaml.Node, depth int) (any, error) {
	if depth > 64 {
		return nil, domain.Invalid("YAML too deep")
	}
	if n.Style&yaml.TaggedStyle != 0 || n.Tag == "!!merge" || n.Kind == yaml.AliasNode {
		return nil, domain.Invalid("YAML tags, merge keys and aliases are not supported")
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, domain.Invalid("empty YAML")
		}
		return yamlValue(n.Content[0], depth)
	case yaml.MappingNode:
		m := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Tag != "!!str" {
				return nil, domain.Invalid("YAML object key must be string")
			}
			if _, ok := m[k.Value]; ok {
				return nil, domain.Invalid("duplicate YAML key")
			}
			v, e := yamlValue(n.Content[i+1], depth+1)
			if e != nil {
				return nil, e
			}
			m[k.Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		a := []any{}
		for _, c := range n.Content {
			v, e := yamlValue(c, depth+1)
			if e != nil {
				return nil, e
			}
			a = append(a, v)
		}
		return a, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool", "!!int", "!!float":
			var v any
			if e := n.Decode(&v); e != nil {
				return nil, domain.Invalid(e.Error())
			}
			return v, nil
		}
	}
	return nil, domain.Invalid(fmt.Sprintf("unsupported YAML value %s", n.Tag))
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
