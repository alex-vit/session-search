// Package main implements session-search, a CLI that full-text searches past
// pi, Claude Code, and Codex sessions.
//
// Data flow:
//
//  1. main → run parses CLI flags and dispatches to a subcommand.
//  2. For search queries, ensureIndex is called first. It checks whether the
//     on-disk index is stale (session JSONL files were added, changed, or
//     removed) and, if so, acquires a file lock and calls buildIndex.
//  3. buildIndex walks the pi (~/.pi/agent/sessions), Claude Code
//     (~/.claude/projects), and Codex (~/.codex/sessions) session directories,
//     diffs the discovered JSONL files against what's already in the index, and
//     appends or refreshes entries as needed. Each entry is a header line
//     ("### SESSION: <path>") followed by extracted content from that session.
//  4. extractSession parses one JSONL file. It extracts user/assistant text,
//     tool outputs (bash, Slack, Honeycomb, web search, etc.), thinking blocks,
//     and bash command inputs. Tool results are selectively indexed by tool
//     name - high-value ephemeral content (bash output, Slack threads) is
//     included while redundant content (file reads, edit confirmations) is
//     skipped.
//  5. searchIndex loads the full index into memory, builds a matcher (literal
//     or regex per query term, OR'd together), and delegates to one of three
//     output modes: context (grep-like with surrounding lines), grouped (by
//     session), or JSON.
//
// The index lives at ~/.config/session-search/index.txt. Concurrent writers are
// serialized via flock on a sibling .lock file.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	sessionPrefix  = "### SESSION: "
	contextLines   = 3
	maxResultBytes = 10 * 1024         // 10 KB per tool result
	maxIndexBytes  = 100 * 1024 * 1024 // 100 MB hard cap
	roleUser       = "user"
	roleAssistant  = "assistant"
	blockText      = "text"
)

var (
	homeDir string
	version = "dev"
)

type sessionFingerprint struct {
	Size      int64
	ModTimeNS int64
}

type sessionFileInfo struct {
	Path        string
	Fingerprint sessionFingerprint
}

func configDir() string {
	return filepath.Join(homeDir, ".config", "session-search")
}

func indexPath() string {
	return filepath.Join(configDir(), "index.txt")
}

func lockPath() string {
	return filepath.Join(configDir(), "index.lock")
}

// sessionSource defines a directory tree to scan for JSONL session files.
// Exclude is a substring filter: any path containing it is skipped.
type sessionSource struct {
	dir     string
	exclude string
}

func sessionSources() []sessionSource {
	return []sessionSource{
		{dir: filepath.Join(homeDir, ".pi", "agent", "sessions")},
		{dir: filepath.Join(homeDir, ".claude", "projects"), exclude: "/subagents/"},
		{dir: filepath.Join(homeDir, ".codex", "sessions")},
	}
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}
	homeDir = home

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}

	var (
		queries []string
		opts    searchOptions
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-v", "--v", "-version", "--version":
			showVersion()
			return nil
		case "--index-only":
			return ensureIndex()
		case "--rebuild":
			start := time.Now()
			fmt.Fprintln(os.Stderr, "Rebuilding index...")
			return withIndexLock(func() error {
				n, err := buildIndex(true)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "Indexed %d sessions in %v.\n", n, time.Since(start).Round(time.Millisecond))
				return nil
			})
		case "--stats":
			showStats()
			return nil
		case "--json":
			opts.jsonOutput = true
		case "--group":
			opts.groupBySession = true
		case "--max", "--limit", "-n":
			if i+1 >= len(args) {
				return errors.New("--max requires a number argument")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				return fmt.Errorf("--max requires a positive integer, got %q", args[i])
			}
			opts.maxResults = n
		case "--help", "-h":
			usage()
			return nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s", args[i])
			}
			queries = append(queries, args[i])
		}
	}

	if len(queries) == 0 {
		usage()
		return nil
	}

	if err := ensureIndex(); err != nil {
		return err
	}

	if _, err := os.Stat(indexPath()); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "No sessions indexed.")
		return nil
	}

	return searchIndex(queries, opts)
}

func usage() {
	fmt.Printf(`session-search %s

search past pi, Claude Code, and Codex sessions

Usage:
    session-search <query> [query2 ...]    Search for terms (OR'd together)
    session-search --index-only            Index new or changed sessions and exit
    session-search --rebuild               Force full index rebuild
    session-search --stats                 Show index stats
    session-search --version               Show version

Options:
    --json          Output results as JSON
    --max N         Limit to N matches (also: --limit, -n)
    --group         Group results by session
    --index-only    Index new or changed sessions without searching
    --rebuild       Force full index rebuild
    --stats         Show index size and session count
    -v, --v, -version, --version
                    Show version
`, version)
}

func showVersion() {
	fmt.Printf("session-search %s\n", version)
}

// ensureIndex brings the index up to date if any session files are new,
// changed, or removed. Safe to call from multiple processes; buildIndex runs
// under flock.
func ensureIndex() error {
	if _, err := os.Stat(indexPath()); os.IsNotExist(err) || needsUpdate() {
		return withIndexLock(func() error {
			n, err := buildIndex(false)
			if err != nil {
				return err
			}
			if n > 0 {
				fmt.Fprintf(os.Stderr, "Indexed %d updated sessions.\n", n)
			}
			return nil
		})
	}
	return nil
}

func needsUpdate() bool {
	allFiles := getAllSessionFileInfos()
	indexed := getIndexedFiles(indexPath())
	if len(allFiles) != len(indexed) {
		return true
	}

	for _, file := range allFiles {
		fingerprint, ok := indexed[file.Path]
		if !ok || fingerprint != file.Fingerprint {
			return true
		}
	}

	return false
}

// withIndexLock acquires an exclusive flock on the index lock file,
// runs fn, then releases the lock. This serializes concurrent index
// operations (e.g. two pi sessions ending simultaneously).
func withIndexLock(fn func() error) error {
	lockFile := lockPath()
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o750); err != nil {
		return fmt.Errorf("cannot create config dir for lock: %w", err)
	}

	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path constructed internally
	if err != nil {
		return fmt.Errorf("cannot open lock file: %w", err)
	}
	defer logClose(f)

	fd := int(f.Fd()) //nolint:gosec // fd fits in int on all supported platforms
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return fmt.Errorf("cannot acquire index lock: %w", err)
	}
	defer syscall.Flock(fd, syscall.LOCK_UN) //nolint:errcheck // best-effort unlock

	return fn()
}

// buildIndex incrementally appends new sessions to the index. If any existing
// session files changed or disappeared, it rewrites the full index so stale
// content is replaced cleanly. This is intentionally blunt but correct for
// now; it keeps search simple at the cost of slower updates when a single
// session file grows.
//
// TODO: Replace the full-index rewrite path with a more incremental changed-
// file update strategy (for example append-only blocks plus metadata/compaction)
// so growing Codex sessions do not force a full rebuild.
//
// Returns the number of new or changed sessions processed. Must be called
// under withIndexLock.
func buildIndex(rebuild bool) (int, error) {
	idxPath := indexPath()
	allFiles := getAllSessionFileInfos()
	indexed := map[string]sessionFingerprint{}
	if !rebuild {
		indexed = getIndexedFiles(idxPath)
	}

	var newFiles []sessionFileInfo
	var changedFiles []sessionFileInfo
	for _, f := range allFiles {
		fingerprint, ok := indexed[f.Path]
		if !ok {
			newFiles = append(newFiles, f)
			continue
		}
		if fingerprint != f.Fingerprint {
			changedFiles = append(changedFiles, f)
		}
	}

	removedFiles := 0
	if !rebuild {
		current := make(map[string]struct{}, len(allFiles))
		for _, f := range allFiles {
			current[f.Path] = struct{}{}
		}
		for path := range indexed {
			if _, ok := current[path]; !ok {
				removedFiles++
			}
		}
	}

	if !rebuild && len(newFiles) == 0 && len(changedFiles) == 0 && removedFiles == 0 {
		return 0, nil
	}

	if err := os.MkdirAll(filepath.Dir(idxPath), 0o750); err != nil {
		return 0, fmt.Errorf("cannot create index directory: %w", err)
	}

	if info, err := os.Stat(idxPath); err == nil && info.Size() > maxIndexBytes {
		fmt.Fprintf(os.Stderr, "Warning: index exceeds %d MB, skipping. Run --rebuild to regenerate.\n", maxIndexBytes/(1024*1024))
		return 0, nil
	}

	if rebuild || len(changedFiles) > 0 || removedFiles > 0 {
		if err := rewriteIndex(idxPath, allFiles); err != nil {
			return 0, err
		}
		if rebuild {
			return len(allFiles), nil
		}
		return len(newFiles) + len(changedFiles), nil
	}

	f, err := os.OpenFile(idxPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path constructed internally
	if err != nil {
		return 0, fmt.Errorf("cannot open index for writing: %w", err)
	}
	defer logClose(f)

	w := bufio.NewWriter(f)
	for _, file := range newFiles {
		text := extractSession(file.Path)
		w.WriteString(formatSessionHeader(file) + "\n")
		if len(text) > 0 {
			w.WriteString(text)
		}
		w.WriteString("\n")
	}
	if err := w.Flush(); err != nil {
		return 0, fmt.Errorf("error writing index: %w", err)
	}

	return len(newFiles), nil
}

func rewriteIndex(idxPath string, files []sessionFileInfo) error {
	tmpPath := idxPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // path constructed internally
	if err != nil {
		return fmt.Errorf("cannot open temp index for writing: %w", err)
	}

	w := bufio.NewWriter(f)
	for _, file := range files {
		text := extractSession(file.Path)
		w.WriteString(formatSessionHeader(file) + "\n")
		if len(text) > 0 {
			w.WriteString(text)
		}
		w.WriteString("\n")
	}
	if err := w.Flush(); err != nil {
		logClose(f)
		return fmt.Errorf("error writing index: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("error closing temp index: %w", err)
	}
	if err := os.Rename(tmpPath, idxPath); err != nil {
		return fmt.Errorf("cannot replace index: %w", err)
	}
	return nil
}

func getAllSessionFileInfos() []sessionFileInfo {
	var files []sessionFileInfo
	for _, src := range sessionSources() {
		info, err := os.Stat(src.dir)
		if err != nil || !info.IsDir() {
			continue
		}
		if err := filepath.Walk(src.dir, func(path string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return filepath.SkipDir
			}
			if fi.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if len(src.exclude) > 0 && strings.Contains(path, src.exclude) {
				return nil
			}
			files = append(files, sessionFileInfo{
				Path: path,
				Fingerprint: sessionFingerprint{
					Size:      fi.Size(),
					ModTimeNS: fi.ModTime().UnixNano(),
				},
			})
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: error walking %s: %v\n", src.dir, err)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

func getAllSessionFiles() []string {
	fileInfos := getAllSessionFileInfos()
	files := make([]string, 0, len(fileInfos))
	for _, fileInfo := range fileInfos {
		files = append(files, fileInfo.Path)
	}
	return files
}

func getIndexedFiles(idxPath string) map[string]sessionFingerprint {
	indexed := make(map[string]sessionFingerprint)
	f, err := os.Open(idxPath) //nolint:gosec // index path is constructed internally
	if err != nil {
		return indexed
	}
	defer logClose(f)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, sessionPrefix) {
			path, fingerprint := parseSessionHeader(line)
			indexed[path] = fingerprint
		}
	}
	return indexed
}

func formatSessionHeader(file sessionFileInfo) string {
	return fmt.Sprintf("%s%s\t%d\t%d", sessionPrefix, file.Path, file.Fingerprint.Size, file.Fingerprint.ModTimeNS)
}

func parseSessionHeader(line string) (string, sessionFingerprint) {
	raw := line[len(sessionPrefix):]
	parts := strings.Split(raw, "\t")
	if len(parts) != 3 {
		return raw, sessionFingerprint{}
	}

	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return raw, sessionFingerprint{}
	}
	modTimeNS, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return raw, sessionFingerprint{}
	}

	return parts[0], sessionFingerprint{Size: size, ModTimeNS: modTimeNS}
}

type jsonlMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role       string `json:"role"`
		Content    any    `json:"content"`
		ToolCallID string `json:"toolCallId"`
	} `json:"message"`
}

type codexRecord struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type codexResponseItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	Content   any    `json:"content"`
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
	CallID    string `json:"call_id"`
	Output    any    `json:"output"`
}

type toolTier int

const (
	tierIndex toolTier = iota
	tierCapped
	tierSkip
)

var toolTiers = map[string]toolTier{
	"bash":          tierIndex,
	"Bash":          tierIndex,
	"Grep":          tierIndex,
	"web_search":    tierIndex,
	"WebSearch":     tierIndex,
	"WebFetch":      tierIndex,
	"fetch_content": tierIndex,
	"code_search":   tierIndex,
	"slack":         tierIndex,
	"honeycomb":     tierIndex,
	"sentry":        tierIndex,
	"linear":        tierIndex,
	"gws":           tierIndex,
	"gccli":         tierIndex,
	"gmcli":         tierIndex,
	"snow":          tierIndex,
	"bamboo":        tierIndex,

	"Agent":         tierCapped,
	"Task":          tierCapped,
	"gdcli":         tierCapped,
	"exec_command":  tierCapped,
	"shell_command": tierCapped,
}

const cappedResultBytes = 2048

func getToolTier(name string) toolTier {
	if tier, ok := toolTiers[name]; ok {
		return tier
	}
	return tierSkip
}

func looksLikeBinary(s string) bool {
	if len(s) < 100 {
		return false
	}
	if strings.ContainsRune(s, '\x00') {
		return true
	}
	for _, line := range strings.SplitN(s, "\n", 20) {
		if len(line) >= 100 && isBase64Line(line) {
			return true
		}
	}
	return false
}

func isBase64Line(s string) bool {
	for _, c := range s {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '+' && c != '/' && c != '=' {
			return false
		}
	}
	return true
}

func truncateResult(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	truncated := s[:maxBytes]
	if idx := strings.LastIndex(truncated, "\n"); idx > maxBytes/2 {
		truncated = truncated[:idx]
	}
	return truncated + "\n[...truncated]"
}

func extractSession(path string) string {
	f, err := os.Open(path) //nolint:gosec // paths come from filesystem walk
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot open session %s: %v\n", path, err)
		return ""
	}
	defer logClose(f)

	var out strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	lastPiToolCalls := map[string]string{}
	ccToolNames := map[string]string{}
	codexToolCalls := map[string]string{}

	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			continue
		}

		var rec codexRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}

		switch rec.Type {
		case "message":
			var msg jsonlMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			extractPiMessage(&msg, &out, lastPiToolCalls)
		case "user":
			var msg jsonlMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			extractCCUser(msg.Message.Content, &out, ccToolNames)
		case "assistant":
			var msg jsonlMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			extractCCAssistant(msg.Message.Content, &out, ccToolNames)
		case "response_item":
			extractCodexResponseItem(rec.Payload, &out, codexToolCalls)
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: error reading session %s: %v\n", path, err)
	}

	return out.String()
}

func extractCodexResponseItem(payload json.RawMessage, out *strings.Builder, toolCalls map[string]string) {
	var item codexResponseItem
	if err := json.Unmarshal(payload, &item); err != nil {
		return
	}

	switch item.Type {
	case "message":
		if item.Role != roleUser && item.Role != roleAssistant {
			return
		}
		text := extractCodexMessageText(item.Content)
		if len(text) > 0 {
			writeBlock(out, item.Role, text)
		}
	case "function_call":
		if len(item.CallID) > 0 && len(item.Name) > 0 {
			toolCalls[item.CallID] = item.Name
		}
		if cmd := extractCodexCommand(item.Arguments); len(cmd) > 0 && isCodexCommandTool(item.Name) {
			writeBlock(out, "bash-cmd", "$ "+cmd)
		}
	case "function_call_output":
		toolName := toolCalls[item.CallID]
		if len(toolName) == 0 {
			return
		}
		tier := getToolTier(toolName)
		if tier == tierSkip {
			return
		}
		text := extractToolResultContent(item.Output)
		if len(text) == 0 || looksLikeBinary(text) {
			return
		}
		text = normalizeCodexToolOutput(toolName, text)
		if tier == tierCapped {
			text = truncateResult(text, cappedResultBytes)
		} else {
			text = truncateResult(text, maxResultBytes)
		}
		writeBlock(out, toolName, text)
	}
}

func extractCodexMessageText(content any) string {
	switch v := content.(type) {
	case []any:
		var parts []string
		for _, block := range v {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := m["type"].(string)
			if blockType != "input_text" && blockType != "output_text" {
				continue
			}
			text, _ := m["text"].(string)
			if len(text) > 0 {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return extractTextContent(content)
	}
}

func extractCodexCommand(arguments any) string {
	switch v := arguments.(type) {
	case map[string]any:
		if cmd, _ := v["cmd"].(string); len(cmd) > 0 {
			return cmd
		}
		if cmd, _ := v["command"].(string); len(cmd) > 0 {
			return cmd
		}
	case string:
		var parsed map[string]any
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			if cmd, _ := parsed["cmd"].(string); len(cmd) > 0 {
				return cmd
			}
			if cmd, _ := parsed["command"].(string); len(cmd) > 0 {
				return cmd
			}
		}
	}
	return ""
}

func isCodexCommandTool(name string) bool {
	return name == "exec_command" || name == "shell_command"
}

func normalizeCodexToolOutput(toolName, text string) string {
	if !isCodexCommandTool(toolName) {
		return text
	}
	separator := "\nOutput:\n"
	if _, after, ok := strings.Cut(text, separator); ok {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(text)
}

func extractPiMessage(msg *jsonlMessage, out *strings.Builder, toolCalls map[string]string) {
	role := msg.Message.Role
	content := msg.Message.Content

	switch role {
	case roleUser:
		text := extractTextContent(content)
		if len(text) > 0 {
			writeBlock(out, roleUser, text)
		}
	case roleAssistant:
		blocks, ok := content.([]any)
		if !ok {
			text := extractTextContent(content)
			if len(text) > 0 {
				writeBlock(out, roleAssistant, text)
			}
			return
		}
		var textParts []string
		for _, block := range blocks {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := m["type"].(string)
			switch blockType {
			case blockText:
				if text, _ := m["text"].(string); len(text) > 0 {
					textParts = append(textParts, text)
				}
			case "thinking":
				if thinking, _ := m["thinking"].(string); len(thinking) > 0 {
					writeBlock(out, "thinking", thinking)
				}
			case "toolCall":
				id, _ := m["id"].(string)
				name, _ := m["name"].(string)
				if len(id) > 0 && len(name) > 0 {
					toolCalls[id] = name
				}
				if name == "bash" || name == "Bash" {
					if args, ok := m["arguments"].(map[string]any); ok {
						if cmd, _ := args["command"].(string); len(cmd) > 0 {
							writeBlock(out, "bash-cmd", "$ "+cmd)
						}
					}
				}
			}
		}
		if len(textParts) > 0 {
			writeBlock(out, roleAssistant, strings.Join(textParts, "\n"))
		}
	case "toolResult":
		toolName := toolCalls[msg.Message.ToolCallID]
		if len(toolName) == 0 {
			return
		}
		tier := getToolTier(toolName)
		if tier == tierSkip {
			return
		}
		text := extractTextContent(content)
		if len(text) == 0 || looksLikeBinary(text) {
			return
		}
		if tier == tierCapped {
			text = truncateResult(text, cappedResultBytes)
		} else {
			text = truncateResult(text, maxResultBytes)
		}
		writeBlock(out, toolName, text)
	}
}

func extractCCAssistant(content any, out *strings.Builder, toolNames map[string]string) {
	blocks, ok := content.([]any)
	if !ok {
		text := extractTextContent(content)
		if len(text) > 0 {
			writeBlock(out, roleAssistant, text)
		}
		return
	}
	var textParts []string
	for _, block := range blocks {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := m["type"].(string)
		switch blockType {
		case blockText:
			if text, _ := m["text"].(string); len(text) > 0 {
				textParts = append(textParts, text)
			}
		case "thinking":
			if thinking, _ := m["thinking"].(string); len(thinking) > 0 {
				writeBlock(out, "thinking", thinking)
			}
		case "tool_use":
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			if len(id) > 0 && len(name) > 0 {
				toolNames[id] = name
			}
			if name == "bash" || name == "Bash" {
				if input, ok := m["input"].(map[string]any); ok {
					if cmd, _ := input["command"].(string); len(cmd) > 0 {
						writeBlock(out, "bash-cmd", "$ "+cmd)
					}
				}
			}
		}
	}
	if len(textParts) > 0 {
		writeBlock(out, roleAssistant, strings.Join(textParts, "\n"))
	}
}

func extractCCUser(content any, out *strings.Builder, toolNames map[string]string) {
	blocks, ok := content.([]any)
	if !ok {
		text := extractTextContent(content)
		if len(text) > 0 {
			writeBlock(out, roleUser, text)
		}
		return
	}

	var textParts []string
	for _, block := range blocks {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := m["type"].(string)
		switch blockType {
		case blockText:
			if text, _ := m["text"].(string); len(text) > 0 {
				textParts = append(textParts, text)
			}
		case "tool_result":
			toolUseID, _ := m["tool_use_id"].(string)
			toolName := toolNames[toolUseID]
			if len(toolName) == 0 {
				continue
			}
			tier := getToolTier(toolName)
			if tier == tierSkip {
				continue
			}
			resultText := extractToolResultContent(m["content"])
			if len(resultText) == 0 || looksLikeBinary(resultText) {
				continue
			}
			if tier == tierCapped {
				resultText = truncateResult(resultText, cappedResultBytes)
			} else {
				resultText = truncateResult(resultText, maxResultBytes)
			}
			writeBlock(out, toolName, resultText)
		}
	}
	if len(textParts) > 0 {
		writeBlock(out, roleUser, strings.Join(textParts, "\n"))
	}
}

func extractToolResultContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, block := range v {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := m["text"].(string); len(text) > 0 {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, _ := v["text"].(string); len(text) > 0 {
			return text
		}
		if contentText, _ := v["content"].(string); len(contentText) > 0 {
			return contentText
		}
	}
	return ""
}

func writeBlock(out *strings.Builder, label, text string) {
	out.WriteString("[")
	out.WriteString(label)
	out.WriteString("]\n")
	out.WriteString(text)
	out.WriteString("\n\n")
}

func extractTextContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, block := range v {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := m["type"].(string)
			if blockType != blockText {
				continue
			}
			text, _ := m["text"].(string)
			if len(text) > 0 {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

type matchFunc func(line string) bool

func buildMatcher(queries []string) matchFunc {
	type matcher struct {
		literal string
		re      *regexp.Regexp
	}

	matchers := make([]matcher, 0, len(queries))
	for _, q := range queries {
		if strings.ContainsAny(q, `\.+*?^${}()|[]`) {
			re, err := regexp.Compile("(?i)" + q)
			if err != nil {
				matchers = append(matchers, matcher{literal: strings.ToLower(q)})
			} else {
				matchers = append(matchers, matcher{re: re})
			}
		} else {
			matchers = append(matchers, matcher{literal: strings.ToLower(q)})
		}
	}

	return func(line string) bool {
		lowered := strings.ToLower(line)
		for _, m := range matchers {
			if m.re != nil {
				if m.re.MatchString(line) {
					return true
				}
			} else if strings.Contains(lowered, m.literal) {
				return true
			}
		}
		return false
	}
}

type searchOptions struct {
	maxResults     int
	groupBySession bool
	jsonOutput     bool
}

func searchIndex(queries []string, opts searchOptions) error {
	data, err := os.ReadFile(indexPath())
	if err != nil {
		return fmt.Errorf("cannot read index: %w", err)
	}
	if len(data) == 0 {
		fmt.Fprintln(os.Stderr, "No sessions indexed.")
		return nil
	}

	lines := strings.Split(string(data), "\n")
	matches := buildMatcher(queries)

	switch {
	case opts.jsonOutput:
		return searchJSON(lines, matches, opts.maxResults)
	case opts.groupBySession:
		searchGrouped(lines, matches, opts.maxResults)
	default:
		searchWithContext(lines, matches, opts.maxResults)
	}
	return nil
}

func searchWithContext(lines []string, matches matchFunc, maxResults int) {
	matchIndices := make([]int, 0, 64)
	for i, line := range lines {
		if strings.HasPrefix(line, sessionPrefix) {
			continue
		}
		if matches(line) {
			matchIndices = append(matchIndices, i)
			if maxResults > 0 && len(matchIndices) >= maxResults {
				break
			}
		}
	}

	if len(matchIndices) == 0 {
		fmt.Fprintln(os.Stderr, "No matches found.")
		return
	}

	printLine := make([]bool, len(lines))
	for _, idx := range matchIndices {
		lo := max(idx-contextLines, 0)
		hi := idx + contextLines
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		for j := lo; j <= hi; j++ {
			printLine[j] = true
		}
	}

	w := bufio.NewWriter(os.Stdout)
	defer flushWriter(w) //nolint:errcheck // stdout

	lastPrinted := -contextLines - 2
	for i, line := range lines {
		if !printLine[i] {
			continue
		}
		if i-lastPrinted > 1 && lastPrinted >= 0 {
			fmt.Fprintln(w, "---")
		}
		fmt.Fprintln(w, line)
		lastPrinted = i
	}
}

type sessionMatch struct {
	session string
	lines   []string
}

func searchGrouped(lines []string, matches matchFunc, maxResults int) {
	var results []sessionMatch
	currentSession := ""
	count := 0

	for _, line := range lines {
		if strings.HasPrefix(line, sessionPrefix) {
			currentSession, _ = parseSessionHeader(line)
			continue
		}
		if matches(line) {
			count++
			if maxResults > 0 && count > maxResults {
				break
			}
			if len(results) > 0 && results[len(results)-1].session == currentSession {
				results[len(results)-1].lines = append(results[len(results)-1].lines, line)
			} else {
				results = append(results, sessionMatch{session: currentSession, lines: []string{line}})
			}
		}
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "No matches found.")
		return
	}

	w := bufio.NewWriter(os.Stdout)
	defer flushWriter(w) //nolint:errcheck // stdout

	for i, r := range results {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "=== %s ===\n", r.session)
		for _, l := range r.lines {
			fmt.Fprintln(w, l)
		}
	}
}

type jsonResult struct {
	Session string   `json:"session"`
	Matches []string `json:"matches"`
}

func searchJSON(lines []string, matches matchFunc, maxResults int) error {
	var results []jsonResult
	currentSession := ""
	count := 0

	for _, line := range lines {
		if strings.HasPrefix(line, sessionPrefix) {
			currentSession, _ = parseSessionHeader(line)
			continue
		}
		if matches(line) {
			count++
			if maxResults > 0 && count > maxResults {
				break
			}
			if len(results) > 0 && results[len(results)-1].Session == currentSession {
				results[len(results)-1].Matches = append(results[len(results)-1].Matches, line)
			} else {
				results = append(results, jsonResult{Session: currentSession, Matches: []string{line}})
			}
		}
	}

	if results == nil {
		results = []jsonResult{}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		return fmt.Errorf("error encoding JSON: %w", err)
	}
	return nil
}

func showStats() {
	idxPath := indexPath()

	info, err := os.Stat(idxPath)
	if os.IsNotExist(err) {
		fmt.Println("No index file found.")
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	indexed := getIndexedFiles(idxPath)
	allFiles := getAllSessionFiles()

	fmt.Printf("Index: %s\n", idxPath)
	fmt.Printf("Size: %.1f MB\n", float64(info.Size())/(1024*1024))
	fmt.Printf("Sessions indexed: %d\n", len(indexed))
	fmt.Printf("Total session files: %d\n", len(allFiles))
	if unindexed := len(allFiles) - len(indexed); unindexed > 0 {
		fmt.Printf("Unindexed: %d\n", unindexed)
	}
}

func logClose(c io.Closer) {
	if err := c.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: close error: %v\n", err)
	}
}

func flushWriter(w *bufio.Writer) error {
	return w.Flush()
}
