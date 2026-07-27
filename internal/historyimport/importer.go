// Package historyimport runs configured transcript importers behind a small,
// versioned NDJSON protocol. Importers are trusted extension code that normalize
// vendor data; Tandem alone owns validation, persistence, FTS, and resume policy.
package historyimport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/store"
)

const (
	ProtocolVersion      = 1
	defaultTimeout       = 2 * time.Minute
	maxLineBytes         = 2 << 20
	maxOutputBytes       = 256 << 20
	maxStderrBytes       = 64 << 10
	maxEntriesPerSession = 100_000
)

type Options struct {
	Store       *store.Store
	Node        string
	RuntimeRoot string
	Timeout     time.Duration
	BaseEnv     []string
}

type Runner struct {
	store   *store.Store
	node    string
	runner  string
	tsx     string
	timeout time.Duration
	baseEnv []string
}

type Result struct {
	Sessions int
	Entries  int
}

func New(options Options) (*Runner, error) {
	if options.Store == nil || options.Node == "" || options.RuntimeRoot == "" {
		return nil, errors.New("history importer requires store, Node, and runtime root")
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultTimeout
	}
	return &Runner{
		store: options.Store, node: options.Node, timeout: options.Timeout,
		runner:  filepath.Join(options.RuntimeRoot, "history", "runner.ts"),
		tsx:     filepath.Join(options.RuntimeRoot, "node_modules", "tsx", "dist", "cli.mjs"),
		baseEnv: options.BaseEnv,
	}, nil
}

type requestCheckpoint struct {
	ImporterID      string          `json:"importerId"`
	ImporterVersion int             `json:"importerVersion"`
	SourceKey       string          `json:"sourceKey"`
	Checkpoint      json.RawMessage `json:"checkpoint"`
}

type request struct {
	ProtocolVersion int                 `json:"protocolVersion"`
	Agent           string              `json:"agent"`
	Checkpoints     []requestCheckpoint `json:"checkpoints"`
}

type helloRecord struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocolVersion"`
	Importer        struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"importer"`
}

type protocolSession struct {
	ID         string          `json:"id"`
	CWD        string          `json:"cwd,omitempty"`
	Title      string          `json:"title,omitempty"`
	CreatedAt  *int64          `json:"createdAt,omitempty"`
	UpdatedAt  *int64          `json:"updatedAt,omitempty"`
	Resumable  *bool           `json:"resumable,omitempty"`
	SourceMeta json.RawMessage `json:"sourceMeta,omitempty"`
}

type beginRecord struct {
	Type      string          `json:"type"`
	Mode      string          `json:"mode"`
	SourceKey string          `json:"sourceKey"`
	Session   protocolSession `json:"session"`
}

type entryRecord struct {
	Type  string `json:"type"`
	Entry struct {
		ID        string `json:"id"`
		Ordinal   int64  `json:"ordinal"`
		Role      string `json:"role,omitempty"`
		Kind      string `json:"kind,omitempty"`
		Timestamp *int64 `json:"timestamp,omitempty"`
		Text      string `json:"text"`
		Truncated bool   `json:"truncated,omitempty"`
	} `json:"entry"`
}

type endRecord struct {
	Type       string          `json:"type"`
	SourceKey  string          `json:"sourceKey"`
	Checkpoint json.RawMessage `json:"checkpoint"`
}

func (r *Runner) Import(ctx context.Context, agentID string, history config.History) (result Result, retErr error) {
	if !history.Enabled {
		return Result{}, nil
	}
	runID, err := r.store.StartHistoryImportRun(agentID)
	if err != nil {
		return Result{}, err
	}
	var importerID string
	defer func() {
		if finishErr := r.store.FinishHistoryImportRun(runID, result.Sessions, result.Entries, retErr); retErr == nil && finishErr != nil {
			retErr = finishErr
		}
		if retErr != nil {
			_ = r.store.MarkHistoryImportError(agentID, importerID, retErr)
		}
	}()

	checkpoints, err := r.store.HistoryImportCheckpoints(agentID)
	if err != nil {
		return result, err
	}
	req := request{ProtocolVersion: ProtocolVersion, Agent: agentID}
	for _, checkpoint := range checkpoints {
		req.Checkpoints = append(req.Checkpoints, requestCheckpoint{
			ImporterID: checkpoint.ImporterID, ImporterVersion: checkpoint.ImporterVersion,
			SourceKey: checkpoint.SourceKey, Checkpoint: checkpoint.Checkpoint,
		})
	}
	requestJSON, err := json.Marshal(req)
	if err != nil {
		return result, err
	}
	requestJSON = append(requestJSON, '\n')

	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	args := []string{r.tsx, r.runner, history.Parser}
	args = append(args, history.Args...)
	cmd := exec.CommandContext(runCtx, r.node, args...)
	cmd.Env = importerEnv(r.baseEnv, history.Env)
	cmd.Dir = filepath.Dir(filepath.Dir(r.runner))
	configureCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return result, err
	}
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start history importer: %w", err)
	}
	var stderrBuf limitedBuffer
	stderrBuf.limit = maxStderrBytes
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		_, _ = io.Copy(&stderrBuf, stderr)
	}()
	if _, err := stdin.Write(requestJSON); err == nil {
		err = stdin.Close()
	}
	if err != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		stderrWG.Wait()
		return result, fmt.Errorf("send history import request: %w", err)
	}

	readErr := r.consume(stdout, agentID, &importerID, &result)
	if readErr != nil {
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	stderrWG.Wait()
	diagnostic := strings.TrimSpace(stderrBuf.String())
	if runCtx.Err() != nil {
		return result, withDiagnostic(fmt.Errorf("history importer: %w", runCtx.Err()), diagnostic)
	}
	if readErr != nil {
		return result, withDiagnostic(readErr, diagnostic)
	}
	if waitErr != nil {
		return result, withDiagnostic(fmt.Errorf("history importer exited: %w", waitErr), diagnostic)
	}
	return result, nil
}

func (r *Runner) consume(reader io.Reader, agentID string, importerID *string, result *Result) error {
	br := bufio.NewReaderSize(reader, 64<<10)
	var total int64
	lineNumber := 0
	helloSeen := false
	var active *beginRecord
	var entries []store.HistoryEntry
	importerVersion := 0
	for {
		line, err := readLimitedLine(br, maxLineBytes)
		if len(line) > 0 {
			lineNumber++
			total += int64(len(line))
			if total > maxOutputBytes {
				return errors.New("history importer output exceeds limit")
			}
			if len(bytes.TrimSpace(line)) == 0 {
				return fmt.Errorf("history importer line %d is blank", lineNumber)
			}
			var envelope struct {
				Type string `json:"type"`
			}
			// The envelope intentionally permits other fields; strict decoding
			// is applied below to the selected concrete record.
			if err := json.Unmarshal(line, &envelope); err != nil {
				return fmt.Errorf("history importer line %d is invalid JSON", lineNumber)
			}
			switch envelope.Type {
			case "hello":
				if helloSeen || active != nil {
					return fmt.Errorf("history importer line %d has misplaced hello", lineNumber)
				}
				var record helloRecord
				if err := decodeStrict(line, &record); err != nil {
					return lineError(lineNumber, err)
				}
				if record.ProtocolVersion != ProtocolVersion || record.Importer.ID == "" || record.Importer.Version < 1 {
					return fmt.Errorf("history importer line %d has unsupported handshake", lineNumber)
				}
				helloSeen = true
				*importerID = record.Importer.ID
				importerVersion = record.Importer.Version
			case "begin_session":
				if !helloSeen || active != nil {
					return fmt.Errorf("history importer line %d has misplaced begin_session", lineNumber)
				}
				var record beginRecord
				if err := decodeStrict(line, &record); err != nil {
					return lineError(lineNumber, err)
				}
				if record.Mode != "replace" || record.SourceKey == "" || record.Session.ID == "" {
					return fmt.Errorf("history importer line %d has invalid begin_session", lineNumber)
				}
				active, entries = &record, nil
			case "entry":
				if active == nil {
					return fmt.Errorf("history importer line %d has entry outside a session", lineNumber)
				}
				var record entryRecord
				if err := decodeStrict(line, &record); err != nil {
					return lineError(lineNumber, err)
				}
				if record.Entry.ID == "" || strings.TrimSpace(record.Entry.Text) == "" {
					return fmt.Errorf("history importer line %d has invalid entry", lineNumber)
				}
				if len(entries) >= maxEntriesPerSession {
					return errors.New("history importer session exceeds entry limit")
				}
				entries = append(entries, store.HistoryEntry{
					ExternalID: record.Entry.ID, Ordinal: record.Entry.Ordinal,
					Role: record.Entry.Role, Kind: record.Entry.Kind, Timestamp: record.Entry.Timestamp,
					Text: record.Entry.Text, Truncated: record.Entry.Truncated,
				})
			case "end_session":
				if active == nil {
					return fmt.Errorf("history importer line %d has end_session outside a session", lineNumber)
				}
				var record endRecord
				if err := decodeStrict(line, &record); err != nil {
					return lineError(lineNumber, err)
				}
				if record.SourceKey != active.SourceKey || len(record.Checkpoint) == 0 || !json.Valid(record.Checkpoint) {
					return fmt.Errorf("history importer line %d has invalid end_session", lineNumber)
				}
				resumable := true
				if active.Session.Resumable != nil {
					resumable = *active.Session.Resumable
				}
				meta := active.Session.SourceMeta
				if len(meta) == 0 {
					meta = json.RawMessage(`{}`)
				}
				session := store.HistorySession{
					Source: "history", Agent: agentID, ExternalID: active.Session.ID,
					CWD: active.Session.CWD, Title: active.Session.Title,
					CreatedAt: active.Session.CreatedAt, UpdatedAt: active.Session.UpdatedAt,
					Resumable: resumable, SourceKey: active.SourceKey, SourceMeta: meta,
				}
				if err := r.store.ImportHistorySession(session, entries, *importerID, importerVersion, record.Checkpoint); err != nil {
					return fmt.Errorf("store imported session %q: %w", active.Session.ID, err)
				}
				result.Sessions++
				result.Entries += len(entries)
				active, entries = nil, nil
			default:
				return fmt.Errorf("history importer line %d has unknown record type %q", lineNumber, envelope.Type)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			break
		}
	}
	if !helloSeen {
		return errors.New("history importer did not send a handshake")
	}
	if active != nil {
		return errors.New("history importer ended during a session")
	}
	return nil
}

func decodeStrict(line []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func lineError(line int, err error) error {
	return fmt.Errorf("history importer line %d: %w", line, err)
}

func readLimitedLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		part, prefix, err := reader.ReadLine()
		if len(out)+len(part)+1 > limit {
			return nil, errors.New("history importer output line exceeds limit")
		}
		out = append(out, part...)
		if !prefix {
			if err == nil {
				out = append(out, '\n')
			}
			return out, err
		}
	}
}

func importerEnv(base []string, overlay map[string]string) []string {
	if base == nil {
		base = os.Environ()
	}
	allowed := map[string]bool{
		"HOME": true, "USERPROFILE": true, "PATH": true, "PATHEXT": true,
		"SystemRoot": true, "WINDIR": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true,
	}
	values := map[string]string{}
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if ok && allowed[key] {
			values[key] = value
		}
	}
	for key, value := range overlay {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func withDiagnostic(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w (stderr: %s)", err, stderr)
}

type limitedBuffer struct {
	bytes.Buffer
	limit int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - int64(b.Len())
	if remaining > 0 {
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return original, nil
}

// ImportAll runs enabled agent importers independently. An error is reported
// per agent and never prevents later agents from being attempted.
func (r *Runner) ImportAll(ctx context.Context, agents map[string]config.Agent) map[string]error {
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	failures := map[string]error{}
	for _, id := range ids {
		history := agents[id].History
		if history == nil || !history.Enabled {
			continue
		}
		if _, err := r.Import(ctx, id, *history); err != nil {
			failures[id] = err
		}
	}
	return failures
}
