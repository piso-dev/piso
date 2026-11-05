package mcp

import (
	"strings"
	"unicode/utf8"
)

// HostScript is display-only: neither the gateway nor the browser executes it.
// File paths are references on the host, never files for the gateway to read.
type HostScript struct {
	Kind     string `json:"kind"`
	Filename string `json:"filename,omitempty"`
	Language string `json:"language"`
	Contents string `json:"contents,omitempty"`
	Path     string `json:"path,omitempty"`
}

func scriptTextValid(s string, max int) bool {
	if !utf8.ValidString(s) || len(s) > max || strings.TrimSpace(s) == "" {
		return false
	}
	for _, r := range s {
		if (r < 32 && r != '\n' && r != '\t' && r != '\r') || r == 127 {
			return false
		}
	}
	return true
}

// workerMount is the worker's name for the shared project directory. The host
// sees the same files under its own path, so any path a person is asked to run
// must be project-relative: a /workspace path is meaningless outside the
// container and reads as "does not exist" to whoever has to act on it.
const workerMount = "/workspace"

// projectRelative rewrites the worker's own name for the project root into the
// form the host can use, and refuses paths that leave the project. The second
// return is the human-readable reason when a path cannot be used.
func projectRelative(p string) (string, string) {
	p = strings.TrimSpace(p)
	if p == workerMount {
		return ".", ""
	}
	if strings.HasPrefix(p, workerMount+"/") {
		p = "." + strings.TrimPrefix(p, workerMount)
	}
	if strings.HasPrefix(p, "/") {
		return "", "path must be project-relative (for example ./.piso/host-actions/result.txt); the host runs from the project root, so an absolute path is not portable"
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", "path must stay inside the project; \"..\" is refused"
		}
	}
	return p, ""
}

func (s *HostScript) valid() bool {
	if !scriptTextValid(s.Language, 80) {
		return false
	}
	switch s.Kind {
	case "inline":
		return scriptTextValid(s.Filename, 300) && scriptTextValid(s.Contents, 20000) && s.Path == ""
	case "file":
		return scriptTextValid(s.Path, 2000) && s.Contents == "" && s.Filename == ""
	}
	return false
}

func hostScriptSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"kind":     map[string]any{"type": "string", "enum": []string{"inline", "file"}},
			"language": map[string]any{"type": "string", "description": "Interpreter, e.g. bash, python, or powershell."},
			"filename": map[string]any{"type": "string", "description": "Suggested filename for inline contents."},
			"contents": map[string]any{"type": "string", "description": "Complete inline script, at most 20000 UTF-8 bytes; never truncated."},
			"path":     map[string]any{"type": "string", "description": "Existing host script: project-relative or verified host path, not an assumed /workspace mount."},
		},
		"required": []string{"kind", "language"},
		"oneOf": []any{
			map[string]any{"properties": map[string]any{"kind": map[string]any{"const": "inline"}}, "required": []string{"filename", "contents"}},
			map[string]any{"properties": map[string]any{"kind": map[string]any{"const": "file"}}, "required": []string{"path"}},
		},
	}
}
