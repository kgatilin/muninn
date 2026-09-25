package sessconn

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

// Claude Code keeps one JSONL file per session at <root>/<slug>/<session>.jsonl,
// where the slug is the project directory with its separators flattened. The
// transcripts of the subagents a session spawned sit a level deeper, under
// <session>/subagents/, and are not read.
//
// Every record repeats the session's context — cwd, gitBranch — so the first
// record that states each is taken.
var claudeReader = reader{
	tool:        "claude-code",
	defaultRoot: homePath(".claude", "projects"),
	session: func(root, path string) (string, bool) {
		rel, err := filepath.Rel(root, path)
		if err != nil || len(strings.Split(rel, string(filepath.Separator))) != 2 {
			return "", false
		}
		return strings.TrimSuffix(filepath.Base(path), ".jsonl"), true
	},
	read: readClaude,
}

type claudeRecord struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	CWD              string `json:"cwd"`
	GitBranch        string `json:"gitBranch"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	// ToolUseResult is set on the "user" record that carries a tool's result.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Message       struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// Attachment is the harness's side channel. One of its types matters here:
	// a message typed while the agent was working is queued, and is recorded
	// as a queued_command attachment and as no user record at all.
	Attachment struct {
		Type        string          `json:"type"`
		CommandMode string          `json:"commandMode"`
		Prompt      json.RawMessage `json:"prompt"`
		Origin      *struct {
			Kind string `json:"kind"`
		} `json:"origin"`
	} `json:"attachment"`
}

type claudeBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Name  string `json:"name"`
	ID    string `json:"id"`
	Input struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Command      string `json:"command"`
	} `json:"input"`
	// ToolUseID and Content are a tool_result's: the call it answers and what
	// the tool printed, a string or a block list.
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

func readClaude(path, id string, keep func(cwd string) bool) (*Session, error) {
	a := assembly{s: &Session{ID: id}}
	kept := true
	// asked are the shell calls that ran muninn, by tool_use id.
	asked := map[string]bool{}
	err := lines(path, func(line []byte) bool {
		var rec claudeRecord
		if json.Unmarshal(line, &rec) != nil || rec.IsSidechain {
			return true
		}
		if a.s.CWD == "" && rec.CWD != "" {
			a.s.CWD = rec.CWD
			if kept = keep(rec.CWD); !kept {
				return false
			}
		}
		at := parseTime(rec.Timestamp)
		if rec.Type == "attachment" {
			// The same channel queues task notifications and subagent reports;
			// only a prompt a person typed opens a turn.
			q := rec.Attachment
			if q.Type == "queued_command" && q.CommandMode == "prompt" && (q.Origin == nil || q.Origin.Kind == "human") {
				a.user(at, claudeTyped(q.Prompt))
			}
			return true
		}
		if rec.Type != "user" && rec.Type != "assistant" {
			return true
		}
		// A detached HEAD is recorded as the branch "HEAD", which names nothing.
		if a.s.Branch == "" && rec.GitBranch != "HEAD" {
			a.s.Branch = rec.GitBranch
		}
		if a.s.Started.IsZero() {
			a.s.Started = at
		}

		switch rec.Type {
		case "user":
			// Meta records are the harness speaking in the user's place: caveats,
			// skill bodies, image placeholders. A compact summary is the model's
			// own retelling of turns already read.
			if len(asked) > 0 {
				_, blocks := claudeContent(rec.Message.Content)
				for _, b := range blocks {
					if b.Type == "tool_result" && asked[b.ToolUseID] {
						delete(asked, b.ToolUseID)
						a.shown(b.Content)
					}
				}
			}
			if rec.IsMeta || rec.IsCompactSummary || len(rec.ToolUseResult) > 0 {
				return true
			}
			a.user(at, claudeTyped(rec.Message.Content))
		case "assistant":
			// A synthetic message is the client reporting an API error or a
			// limit in the assistant's place.
			if rec.Message.Model == "<synthetic>" {
				return true
			}
			_, blocks := claudeContent(rec.Message.Content)
			for _, b := range blocks {
				switch {
				case b.Type == "text":
					a.reply(at, b.Text)
				case b.Type != "tool_use":
				case b.Name == "Bash":
					if asksMuninn([]byte(b.Input.Command)) {
						asked[b.ID] = true
					}
				case b.Name == "Read":
					a.reads(b.Input.FilePath)
				case b.Name == "Edit" || b.Name == "Write" || b.Name == "MultiEdit":
					a.edits(b.Input.FilePath)
				case b.Name == "NotebookEdit":
					a.edits(b.Input.NotebookPath)
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

// claudeTyped is the typed text of a user message's content, or "" when the
// content is a tool's result or all of it is the harness's.
func claudeTyped(raw json.RawMessage) string {
	text, blocks := claudeContent(raw)
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return ""
		}
		if b.Type == "text" {
			text = append(text, b.Text)
		}
	}
	var typed []string
	for _, t := range text {
		if t = claudeUserText(t); t != "" {
			typed = append(typed, t)
		}
	}
	return strings.Join(typed, "\n\n")
}

// claudeContent reads message.content, which is a string or a block list.
func claudeContent(raw json.RawMessage) (text []string, blocks []claudeBlock) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil
	}
	_ = json.Unmarshal(raw, &blocks)
	return nil, blocks
}

var (
	// injectedRE are the blocks the harness puts inside a user message.
	injectedRE = regexp.MustCompile(`(?s)<(system-reminder|user-prompt-submit-hook)>.*?</(system-reminder|user-prompt-submit-hook)>`)
	commandRE  = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	argsRE     = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// harnessPrefixes open a user record the user did not write: the output of a
// local command, a shell escape, a background task reporting back, a stop.
var harnessPrefixes = []string{
	"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>",
	"<bash-input>", "<bash-stdout>", "<bash-stderr>",
	"<task-notification>",
	"[Request interrupted by user",
}

// claudeUserText is what the user typed, or "" when the text is the harness's.
// A slash command is a wrapper around what was typed: /clear and its kind say
// nothing and are dropped, one with arguments is kept as "/name arguments".
func claudeUserText(text string) string {
	text = strings.TrimSpace(injectedRE.ReplaceAllString(text, ""))
	for _, prefix := range harnessPrefixes {
		if strings.HasPrefix(text, prefix) {
			return ""
		}
	}
	if strings.HasPrefix(text, "<command-name>") || strings.HasPrefix(text, "<command-message>") {
		name := commandRE.FindStringSubmatch(text)
		args := argsRE.FindStringSubmatch(text)
		if name == nil || args == nil || strings.TrimSpace(args[1]) == "" {
			return ""
		}
		return strings.TrimSpace(name[1]) + " " + strings.TrimSpace(args[1])
	}
	return text
}
