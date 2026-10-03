// Package schemaartifact reads SQL line markers without interpreting SQL grammar.
package schemaartifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const prefix = "-- ocservia:"

type Step struct {
	Name     string
	SQL      []byte
	Metadata json.RawMessage
	Checksum [32]byte
}

type Revision struct {
	Number   int64
	Steps    []Step
	Checksum [32]byte
}

type Checkpoint struct {
	Epoch, Revision int64
	Ref             string
}

type Artifact struct {
	Kind, Engine string
	Epoch        int64
	Baseline     Revision
	Revisions    []Revision
	Previous     *Checkpoint
	Transition   *Revision
	Checksum     [32]byte
}

var stepName = regexp.MustCompile(`^[0-9]{3}:[a-z][a-z0-9_]{0,63}$`)

func number(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != s {
		return 0, fmt.Errorf("invalid artifact number %q", s)
	}
	return n, nil
}

// Parse copies its input so callers cannot change SQL after checksum validation.
// Checksums cover raw bytes, including LF endings and revision step metadata.
// Reserved markers must start at column zero; CRLF and unknown markers fail.
func Parse(data []byte, engine string) (Artifact, error) {
	a := Artifact{}
	if engine != "postgresql" && engine != "mysql" {
		return a, errors.New("unsupported artifact engine")
	}
	if len(data) == 0 || data[len(data)-1] != '\n' || bytes.ContainsRune(data, '\r') {
		return a, errors.New("artifact requires LF-terminated lines")
	}
	data = bytes.Clone(data)
	a.Checksum = sha256.Sum256(data)
	headers := map[string]string{}
	header := true
	var rev *Revision
	var current *Step
	revStart, sqlStart := 0, 0
	transition := false
	finishHeader := func() error {
		if !header {
			return nil
		}
		header = false
		a.Kind, a.Engine = headers["artifact"], headers["engine"]
		if (a.Kind != "schema" && a.Kind != "upgrade") || headers["format"] != "1" || a.Engine != engine {
			return errors.New("artifact kind, format or engine mismatch")
		}
		var err error
		a.Epoch, err = number(headers["epoch"])
		if err != nil || a.Epoch == 0 {
			return errors.New("invalid artifact epoch")
		}
		if a.Kind == "schema" {
			if headers["revision"] != "0" || len(headers) != 5 {
				return errors.New("schema requires revision zero")
			}
			rev = &a.Baseline
			return nil
		}
		if _, ok := headers["revision"]; ok {
			return errors.New("upgrade has no baseline header")
		}
		if len(headers) == 4 {
			return nil
		}
		if len(headers) != 7 {
			return errors.New("incomplete previous checkpoint")
		}
		e, err := number(headers["previous-checkpoint-epoch"])
		if err != nil || e == 0 || e != a.Epoch-1 {
			return errors.New("invalid previous checkpoint epoch")
		}
		r, err := number(headers["previous-checkpoint-revision"])
		if err != nil {
			return err
		}
		ref := headers["previous-checkpoint-ref"]
		if ref == "" || strings.ContainsAny(ref, " \t") {
			return errors.New("invalid checkpoint ref")
		}
		a.Previous = &Checkpoint{Epoch: e, Revision: r, Ref: ref}
		return nil
	}
	for offset := 0; offset < len(data); {
		end := offset + bytes.IndexByte(data[offset:], '\n') + 1
		line := string(data[offset : end-1])
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") && strings.Contains(trimmed, "ocservia:") && !strings.HasPrefix(line, prefix) {
			return a, fmt.Errorf("malformed marker at byte %d", offset)
		}
		if !strings.HasPrefix(line, prefix) {
			if current == nil && trimmed != "" {
				return a, fmt.Errorf("SQL outside step at byte %d", offset)
			}
			offset = end
			continue
		}
		marker := strings.TrimPrefix(line, prefix)
		key, value, hasValue := strings.Cut(marker, "=")
		if header && key != "step" && key != "transition" && !(key == "revision" && headers["artifact"] == "upgrade") {
			switch key {
			case "artifact", "format", "engine", "epoch", "revision", "previous-checkpoint-epoch", "previous-checkpoint-revision", "previous-checkpoint-ref":
			default:
				return a, fmt.Errorf("unknown header %q", key)
			}
			if !hasValue || value == "" {
				return a, errors.New("empty artifact header")
			}
			if _, exists := headers[key]; exists {
				return a, fmt.Errorf("duplicate header %q", key)
			}
			headers[key] = value
			offset = end
			continue
		}
		if err := finishHeader(); err != nil {
			return a, err
		}
		switch key {
		case "revision", "transition":
			if a.Kind != "upgrade" || rev != nil || current != nil || !hasValue {
				return a, errors.New("unexpected revision boundary")
			}
			if key == "transition" {
				if a.Previous == nil || a.Transition != nil || len(a.Revisions) != 0 || value != fmt.Sprintf("%d:%d->%d:0", a.Previous.Epoch, a.Previous.Revision, a.Epoch) {
					return a, errors.New("invalid checkpoint transition")
				}
				a.Transition = &Revision{}
				rev, transition = a.Transition, true
			} else {
				n, err := number(value)
				if err != nil || n != int64(len(a.Revisions)+1) {
					return a, errors.New("revisions must be contiguous from one")
				}
				a.Revisions = append(a.Revisions, Revision{Number: n})
				rev = &a.Revisions[len(a.Revisions)-1]
			}
			revStart = end
		case "step":
			if rev == nil || current != nil || !hasValue || !stepName.MatchString(value) {
				return a, errors.New("invalid step boundary")
			}
			// ponytail: format v1 permits at most 999 steps per revision.
			if len(rev.Steps) >= 999 || value[:3] != fmt.Sprintf("%03d", len(rev.Steps)+1) {
				return a, errors.New("steps must be contiguous from 001")
			}
			for _, s := range rev.Steps {
				if s.Name[4:] == value[4:] {
					return a, errors.New("duplicate step name")
				}
			}
			rev.Steps = append(rev.Steps, Step{Name: value})
			current = &rev.Steps[len(rev.Steps)-1]
			sqlStart = end
		case "metadata":
			if current == nil || current.Metadata != nil || sqlStart != offset || !hasValue || !json.Valid([]byte(value)) || !strings.HasPrefix(value, "{") {
				return a, errors.New("metadata must be one JSON object immediately after step")
			}
			current.Metadata = json.RawMessage(value)
			sqlStart = end
		case "end-step":
			if current == nil || hasValue || len(bytes.TrimSpace(data[sqlStart:offset])) == 0 {
				return a, errors.New("empty or unbalanced SQL step")
			}
			current.SQL = data[sqlStart:offset]
			current.Checksum = sha256.Sum256(current.SQL)
			current = nil
		case "end-revision", "end-transition":
			if a.Kind != "upgrade" || rev == nil || current != nil || len(rev.Steps) == 0 || hasValue || (key == "end-transition") != transition {
				return a, errors.New("unbalanced revision boundary")
			}
			rev.Checksum = sha256.Sum256(data[revStart:offset])
			rev, transition = nil, false
		default:
			return a, fmt.Errorf("unknown marker %q", key)
		}
		offset = end
	}
	if err := finishHeader(); err != nil {
		return a, err
	}
	if current != nil || (a.Kind == "upgrade" && rev != nil) || (a.Kind == "schema" && len(a.Baseline.Steps) == 0) || (a.Previous != nil && a.Transition == nil) {
		return a, errors.New("incomplete artifact")
	}
	if a.Kind == "schema" {
		a.Baseline.Checksum = a.Checksum
	}
	return a, nil
}
