// Package corpus extracts plain-text turns from Claude Code session
// transcripts (`~/.claude/projects/<project>/<sessionUUID>.jsonl`).
//
// One JSONL file = one session. Each line is a record. We care about
// type=user and type=assistant; everything else (file-history-snapshot,
// system, queue-operation, sidechain) is skipped.
package corpus

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Turn is one extracted user or assistant turn ready for chunking +
// embedding. SourceFile + LineNumber identify the JSONL line for
// traceability.
type Turn struct {
	Role        string    // "user" | "assistant"
	Text        string    // concatenated text content (no thinking, no tool I/O)
	Timestamp   time.Time // wall-clock from the JSONL record
	SessionID   string
	UUID        string
	ParentUUID  string
	Sidechain   bool
	SourceFile  string
	LineNumber  int
	GitBranch   string
}

// Extract walks `path` (file or dir) and invokes fn for every Turn
// extracted in source order. On a directory, all *.jsonl files are
// processed in lexicographic order; each file is streamed line by line
// so memory is O(turn) not O(corpus).
func Extract(path string, fn func(Turn) error) error {
	if path == "" {
		return errors.New("corpus: path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("corpus: stat %q: %w", path, err)
	}
	if !info.IsDir() {
		return extractFile(path, fn)
	}
	files, err := listJSONL(path)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := extractFile(f, fn); err != nil {
			return err
		}
	}
	return nil
}

// ListJSONLFiles returns the *.jsonl files Extract would process for
// path (file or dir). Useful for sizing progress bars in advance.
func ListJSONLFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("corpus: stat %q: %w", path, err)
	}
	if !info.IsDir() {
		if !strings.EqualFold(filepath.Ext(path), ".jsonl") {
			return nil, fmt.Errorf("corpus: %q is not a .jsonl file", path)
		}
		return []string{path}, nil
	}
	return listJSONL(path)
}

func listJSONL(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("corpus: read dir %q: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".jsonl") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func extractFile(path string, fn func(Turn) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("corpus: open %q: %w", path, err)
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	line := 0
	for {
		raw, err := r.ReadBytes('\n')
		line++
		if len(raw) > 0 {
			turn, ok, perr := parseLine(raw)
			if perr != nil {
				return fmt.Errorf("corpus: %s line %d: %w", path, line, perr)
			}
			if ok {
				turn.SourceFile = path
				turn.LineNumber = line
				if err := fn(turn); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("corpus: read %q: %w", path, err)
		}
	}
}

// claude-code session schema (lossy view, we only decode what we read):
//
//   { "type": "user"|"assistant"|"file-history-snapshot"|"system"|"queue-operation",
//     "isSidechain": bool,
//     "uuid": "...",
//     "parentUuid": "...",
//     "sessionId": "...",
//     "timestamp": "RFC3339",
//     "gitBranch": "...",
//     "message": {
//       "role": "user"|"assistant",
//       "content": string | [ { "type": "text"|"thinking"|"tool_use"|"tool_result", "text": "..." } ]
//     }
//   }

type rawRecord struct {
	Type        string         `json:"type"`
	IsSidechain bool           `json:"isSidechain"`
	UUID        string         `json:"uuid"`
	ParentUUID  string         `json:"parentUuid"`
	SessionID   string         `json:"sessionId"`
	Timestamp   string         `json:"timestamp"`
	GitBranch   string         `json:"gitBranch"`
	Message     *rawMessage    `json:"message"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type rawContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseLine(raw []byte) (Turn, bool, error) {
	raw = trimNewline(raw)
	if len(raw) == 0 {
		return Turn{}, false, nil
	}
	var rec rawRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Turn{}, false, fmt.Errorf("decode record: %w", err)
	}
	switch rec.Type {
	case "user", "assistant":
	default:
		return Turn{}, false, nil
	}
	if rec.IsSidechain {
		return Turn{}, false, nil
	}
	if rec.Message == nil {
		return Turn{}, false, nil
	}
	text, err := extractText(rec.Message.Content)
	if err != nil {
		return Turn{}, false, fmt.Errorf("decode content: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Turn{}, false, nil
	}
	ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
	return Turn{
		Role:       rec.Type,
		Text:       text,
		Timestamp:  ts,
		SessionID:  rec.SessionID,
		UUID:       rec.UUID,
		ParentUUID: rec.ParentUUID,
		Sidechain:  rec.IsSidechain,
		GitBranch:  rec.GitBranch,
	}, true, nil
}

// extractText decodes the polymorphic `message.content` field. User
// messages carry a string OR an array of blocks; assistant messages
// always carry an array. We collect only `text` blocks. Thinking,
// tool_use, and tool_result blocks are intentionally skipped — they're
// either internal monologue or noisy structured data.
func extractText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	if raw[0] == '[' {
		var blocks []rawContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return "", err
		}
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type != "text" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(blk.Text)
		}
		return b.String(), nil
	}
	return "", nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
