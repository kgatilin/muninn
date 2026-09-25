package sessconn

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// Codex keeps one rollout per session at
// <root>/YYYY/MM/DD/rollout-<timestamp>-<session uuid>.jsonl. The session's
// context — cwd, git branch — is stated once, in a session_meta header.
//
// A rollout holds two parallel accounts of the session and one is read.
// `response_item` is the raw API stream, where the harness's preambles
// (environment, instructions) arrive as "user" messages beside the real ones.
// `event_msg` is what the harness made of it: user_message there is only what
// the user typed. So event_msg is read and response_item is passed over.
//
// event_msg has two vocabularies that never share a file. Up to CLI 0.146 it
// is one event per kind: user_message, agent_message, patch_apply_end. From
// 0.147 it is item_completed carrying a typed item: UserMessage, AgentMessage,
// FileChange. Both are read.
//
// File edges come from applied patches only. A Codex read is a shell command
// (cat, sed, rg), and which of its words is a file is a guess, so there are no
// `reads` edges.
var codexReader = reader{
	tool:        "codex",
	defaultRoot: homePath(".codex", "sessions"),
	session: func(_, path string) (string, bool) {
		return codexSessionID(strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	},
	read: readCodex,
}

// codexSessionID takes the uuid off the end of rollout-<timestamp>-<uuid>. The
// timestamp has hyphens of its own, so the uuid is the last five groups.
func codexSessionID(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "rollout-")
	if !ok {
		return "", false
	}
	parts := strings.Split(rest, "-")
	if len(parts) < 6 {
		return "", false
	}
	groups := parts[len(parts)-5:]
	for i, want := range []int{8, 4, 4, 4, 12} {
		if len(groups[i]) != want {
			return "", false
		}
	}
	return strings.Join(groups, "-"), true
}

type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexMeta struct {
	CWD string `json:"cwd"`
	// Source is "cli", "exec" or "vscode" for a session a person or a script
	// started, and an object for one Codex spawned itself.
	Source json.RawMessage `json:"source"`
	Git    struct {
		Branch string `json:"branch"`
	} `json:"git"`
}

type codexEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	// Success is absent in some versions of patch_apply_end; only a stated
	// failure drops the patch.
	Success *bool           `json:"success"`
	Changes json.RawMessage `json:"changes"`
	Item    struct {
		Type    string          `json:"type"`
		Status  string          `json:"status"`
		Content json.RawMessage `json:"content"`
		Changes json.RawMessage `json:"changes"`
	} `json:"item"`
}

var responseItem = []byte(`"type":"response_item"`)

func readCodex(path, id string, keep func(cwd string) bool) (*Session, error) {
	a := assembly{s: &Session{ID: id}}
	kept := true
	// asked are the tool calls that ran muninn, by call_id.
	asked := map[string]bool{}
	err := lines(path, func(line []byte) bool {
		// response_item is most of a rollout's bytes and nearly none of what is
		// read: a call that ran muninn, and the output of one still open. The
		// type sits at the head of the line, before the payload.
		if bytes.Contains(line[:min(len(line), 160)], responseItem) && len(asked) == 0 && !asksMuninn(line) {
			return true
		}
		var rec codexRecord
		if json.Unmarshal(line, &rec) != nil {
			return true
		}
		at := parseTime(rec.Timestamp)
		switch rec.Type {
		case "session_meta", "turn_context":
			var meta codexMeta
			if json.Unmarshal(rec.Payload, &meta) != nil {
				return true
			}
			// A session Codex spawned — the guardian that assesses a planned
			// action, a review, a spawned thread — has a prompt no person
			// wrote. It is the counterpart of a Claude Code subagent.
			if rec.Type == "session_meta" && bytes.HasPrefix(bytes.TrimSpace(meta.Source), []byte("{")) {
				kept = false
				return false
			}
			if a.s.CWD == "" && meta.CWD != "" {
				a.s.CWD = meta.CWD
				if kept = keep(meta.CWD); !kept {
					return false
				}
			}
			if a.s.Branch == "" && meta.Git.Branch != "HEAD" {
				a.s.Branch = meta.Git.Branch
			}
			if a.s.Started.IsZero() {
				a.s.Started = at
			}
		case "response_item":
			// A tool call and its output are two items tied by call_id, under
			// several names (function_call, custom_tool_call, local_shell_call).
			var item struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(rec.Payload, &item) != nil || item.CallID == "" {
				return true
			}
			switch {
			case strings.HasSuffix(item.Type, "_call"):
				if asksMuninn(rec.Payload) {
					asked[item.CallID] = true
				}
			case strings.HasSuffix(item.Type, "_call_output") && asked[item.CallID]:
				delete(asked, item.CallID)
				a.shown(rec.Payload)
			}
		case "event_msg":
			var ev codexEvent
			if json.Unmarshal(rec.Payload, &ev) != nil {
				return true
			}
			switch ev.Type {
			case "user_message":
				a.user(at, codexUserText(ev.Message))
			case "agent_message":
				a.reply(at, ev.Message)
			case "patch_apply_end":
				if ev.Success == nil || *ev.Success {
					codexEdits(&a, ev.Changes)
				}
			case "item_completed":
				switch ev.Item.Type {
				case "UserMessage":
					a.user(at, codexUserText(codexItemText(ev.Item.Content)))
				case "AgentMessage":
					a.reply(at, codexItemText(ev.Item.Content))
				case "FileChange":
					if ev.Item.Status == "" || ev.Item.Status == "completed" {
						codexEdits(&a, ev.Item.Changes)
					}
				}
			}
		}
		return true
	})
	if !kept {
		return nil, err
	}
	return a.s, err
}

// codexItemText joins the text blocks of a typed item's content. Images and
// skill references are other block types and carry no text.
func codexItemText(raw json.RawMessage) string {
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// codexPreambles open a message the harness wrote. event_msg is not where
// these are recorded, so this is a guard for a version that changes that.
var codexPreambles = []string{"<environment_context>", "<user_instructions>", "<turn_aborted>", "# AGENTS.md instructions"}

func codexUserText(text string) string {
	text = strings.TrimSpace(text)
	for _, prefix := range codexPreambles {
		if strings.HasPrefix(text, prefix) {
			return ""
		}
	}
	return text
}

// codexEdits reads a patch's targets: a map from path to the change, with the
// new path of a moved file stated inside the change.
func codexEdits(a *assembly, raw json.RawMessage) {
	var changes map[string]struct {
		MovePath *string `json:"move_path"`
	}
	if json.Unmarshal(raw, &changes) != nil {
		return
	}
	paths := make([]string, 0, len(changes))
	for path, change := range changes {
		paths = append(paths, path)
		if change.MovePath != nil {
			paths = append(paths, *change.MovePath)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		a.edits(path)
	}
}
