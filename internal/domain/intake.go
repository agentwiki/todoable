package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

type Fault struct {
	Code          int
	Kind, Message string
}

func (e *Fault) Error() string     { return e.Message }
func Invalid(message string) error { return &Fault{2, "validation_error", message} }

var taskID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func ValidID(s string) bool { return taskID.MatchString(s) }
func ValidKey(s string) bool {
	return utf8.ValidString(s) && len(s) > 0 && len(s) <= 512 && !strings.ContainsRune(s, 0) && strings.TrimSpace(s) == s
}

// Canonical validates the original token stream before JCS binary64 conversion.
func Canonical(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, Invalid("JSON must be UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := walk(d, 0); err != nil {
		return nil, Invalid(err.Error())
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, Invalid("expected one JSON value")
	}
	out, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	return out, nil
}
func walk(d *json.Decoder, depth int) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch v := t.(type) {
	case json.Delim:
		if depth >= 64 {
			return fmt.Errorf("maximum JSON depth is 64")
		}
		seen := map[string]bool{}
		for d.More() {
			if v == '{' {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return fmt.Errorf("duplicate object field")
				}
				seen[s] = true
			}
			if e := walk(d, depth+1); e != nil {
				return e
			}
		}
		_, err = d.Token()
		return err
	case json.Number:
		if _, e := strconv.ParseFloat(string(v), 64); e != nil {
			return fmt.Errorf("unsupported number")
		}
		r, ok := new(big.Rat).SetString(string(v))
		if !ok {
			return fmt.Errorf("invalid number")
		}
		if r.IsInt() && new(big.Int).Abs(r.Num()).Cmp(big.NewInt(9007199254740991)) > 0 {
			return fmt.Errorf("integer outside safe range")
		}
	}
	return nil
}
func Decode(raw []byte, value any) error {
	c, e := Canonical(raw)
	if e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(c))
	d.DisallowUnknownFields()
	if e = d.Decode(value); e != nil {
		return Invalid(e.Error())
	}
	return nil
}

type SubmissionInput struct {
	TaskID         string          `json:"task_id"`
	TaskVersion    *int            `json:"task_version,omitempty"`
	InputKey       string          `json:"input_key"`
	Input          json.RawMessage `json:"input"`
	ConcurrencyKey string          `json:"concurrency_key"`
}

func ParseSubmission(raw []byte) (SubmissionInput, error) {
	var in SubmissionInput
	if len(raw) > 1048576+16384 {
		return in, Invalid("submission too large")
	}
	if e := Decode(raw, &in); e != nil {
		return in, e
	}
	// Check original input size, before canonical whitespace removal.
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(raw, &fields); e != nil {
		return in, Invalid(e.Error())
	}
	if len(fields["input"]) > 1048576 || len(in.Input) > 1048576 {
		return in, Invalid("input too large")
	}
	if !ValidID(in.TaskID) || !ValidKey(in.InputKey) || !ValidKey(in.ConcurrencyKey) || len(in.Input) == 0 || in.Input[0] != '{' || (in.TaskVersion != nil && *in.TaskVersion < 1) {
		return in, Invalid("invalid submission fields")
	}
	return in, nil
}
func (in SubmissionInput) Hash() string {
	raw, _ := json.Marshal([]any{in.TaskID, in.InputKey, in.Input})
	c, _ := Canonical(raw)
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:])
}

type Finish struct {
	Check    []string `json:"check"`
	MaxCalls int      `json:"max_calls"`
}
type Start struct {
	Check       []string `json:"check"`
	PollEvery   string   `json:"poll_every"`
	WaitTimeout string   `json:"wait_timeout"`
}
type Task struct {
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	Workdir     string            `json:"workdir"`
	Agent       []string          `json:"agent"`
	Prompt      string            `json:"prompt"`
	Before      []string          `json:"before,omitempty"`
	After       []string          `json:"after,omitempty"`
	Env         map[string]string `json:"env"`
	InheritEnv  []string          `json:"inherit_env"`
	Start       *Start            `json:"start,omitempty"`
	Finish      Finish            `json:"finish"`
	Repeat      int               `json:"repeat"`
	RepeatDelay string            `json:"repeat_delay"`
	Limits      map[string]string `json:"limits"`
}
type Result struct {
	ProtocolVersion int    `json:"protocol_version"`
	SubmissionID    string `json:"submission_id"`
	TaskVersion     int    `json:"task_version"`
	Deduplicated    bool   `json:"deduplicated"`
	State           string `json:"state"`
	RunID           string `json:"run_id"`
}
