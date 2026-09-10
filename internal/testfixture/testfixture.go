// Package testfixture builds synthetic transcript lines.
//
// Every fixture in this repository comes from here, and none of them comes from
// a real transcript. That is not a style preference: a real transcript carries
// prompts, source code and customer identifiers, and this archive's whole
// purpose is that those do not leak. See CONTRIBUTING.md.
//
// The builders are deliberately dumb -- they assemble the envelope shape the
// parser reads and nothing else -- so a fixture is readable as data rather than
// as the output of a program.
package testfixture

import (
	"encoding/json"
	"fmt"
)

// Provenance is where a line says it was written: the working directory, and
// the branch checked out there.
//
// Its own type because these two travel together on every line that has either,
// and because a project is derived from the first of them.
type Provenance struct {
	CWD    string `json:"cwd,omitempty"`
	Branch string `json:"gitBranch,omitempty"`
}

// Usage is one request's token counts, in the wire shape the transcript uses.
type Usage struct {
	Input        int64
	Output       int64
	Thinking     int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

// Assistant is a billable assistant response.
//
// One content block per line, every line of a response repeating the whole
// usage object -- which is the shape that makes the requestId fold necessary.
func Assistant(sessionID, uuid, requestID, model, timestamp string, p Provenance, u Usage) string {
	return line(map[string]any{
		"type":        "assistant",
		"sessionId":   sessionID,
		"uuid":        uuid,
		"requestId":   requestID,
		"timestamp":   timestamp,
		"cwd":         p.CWD,
		"gitBranch":   p.Branch,
		"message":     map[string]any{"model": model, "usage": usageShape(u)},
		"isSidechain": false,
	})
}

// Synthetic is an assistant line for an API error: all-zero usage and a model
// of "<synthetic>", which must never reach a cost calculation.
func Synthetic(sessionID, uuid, requestID, timestamp string, p Provenance) string {
	return line(map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"uuid":      uuid,
		"requestId": requestID,
		"timestamp": timestamp,
		"cwd":       p.CWD,
		"gitBranch": p.Branch,
		"message":   map[string]any{"model": "<synthetic>", "usage": usageShape(Usage{})},
	})
}

// User is a prompt: provenance, no usage.
func User(sessionID, uuid, timestamp string, p Provenance) string {
	return line(map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"uuid":      uuid,
		"timestamp": timestamp,
		"cwd":       p.CWD,
		"gitBranch": p.Branch,
		"message":   map[string]any{"role": "user"},
	})
}

// PRLink binds a session to a pull request it opened.
func PRLink(sessionID, uuid, timestamp, repo string, number int64, p Provenance) string {
	return line(map[string]any{
		"type":         "pr-link",
		"sessionId":    sessionID,
		"uuid":         uuid,
		"timestamp":    timestamp,
		"cwd":          p.CWD,
		"gitBranch":    p.Branch,
		"prRepository": repo,
		"prNumber":     number,
	})
}

// Sidecar is one of the lines that carry no timestamp, no provenance and no
// usage. They are stored anyway, and the first and last line of a real file are
// usually two of them.
func Sidecar(sessionID, kind string, fields map[string]any) string {
	l := map[string]any{"type": kind, "sessionId": sessionID}
	for k, v := range fields {
		l[k] = v
	}
	return line(l)
}

func usageShape(u Usage) map[string]any {
	return map[string]any{
		"input_tokens":            u.Input,
		"output_tokens":           u.Output,
		"cache_read_input_tokens": u.CacheRead,
		"output_tokens_details":   map[string]any{"thinking_tokens": u.Thinking},
		"cache_creation": map[string]any{
			"ephemeral_5m_input_tokens": u.CacheWrite5m,
			"ephemeral_1h_input_tokens": u.CacheWrite1h,
		},
	}
}

// line renders one JSONL record, dropping the fields a real line would omit
// rather than send empty.
func line(fields map[string]any) string {
	for k, v := range fields {
		if s, ok := v.(string); ok && s == "" {
			delete(fields, k)
		}
	}
	buf, err := json.Marshal(fields)
	if err != nil {
		// The inputs are all plain values, so this is a programmer error.
		panic(fmt.Sprintf("testfixture: marshal line: %v", err))
	}
	return string(buf)
}
