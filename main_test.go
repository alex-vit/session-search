package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// captureStdout redirects os.Stdout to a pipe, calls fn, then returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("closing stdout pipe: %v", err)
	}
	os.Stdout = old
	buf, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading pipe: %v", err)
	}
	return string(buf)
}

// captureStderr redirects os.Stderr to a pipe, calls fn, then returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("closing stderr pipe: %v", err)
	}
	os.Stderr = old
	buf, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading pipe: %v", err)
	}
	return string(buf)
}

func TestExtractTextContent(t *testing.T) {
	tests := []struct {
		name    string
		content any
		want    string
	}{
		{"plain string", "hello world", "hello world"},
		{"empty string", "", ""},
		{"nil", nil, ""},
		{"number", 42.0, ""},
		{
			"text blocks",
			[]any{
				map[string]any{"type": "text", "text": "first"},
				map[string]any{"type": "text", "text": "second"},
			},
			"first\nsecond",
		},
		{
			"mixed blocks - only text extracted",
			[]any{
				map[string]any{"type": "text", "text": "visible"},
				map[string]any{"type": "thinking", "thinking": "hidden"},
				map[string]any{"type": "tool_use", "name": "bash"},
			},
			"visible",
		},
		{
			"empty text blocks skipped",
			[]any{
				map[string]any{"type": "text", "text": ""},
				map[string]any{"type": "text", "text": "only this"},
			},
			"only this",
		},
		{"empty array", []any{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTextContent(tt.content)
			if got != tt.want {
				t.Errorf("extractTextContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func writeTempJSONL(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractSession_PiFormat(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"session","version":3,"id":"abc"}`,
		`{"type":"message","message":{"role":"user","content":"hello pi"}}`,
		`{"type":"message","message":{"role":"assistant","content":"hi there"}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "[user]\nhello pi") {
		t.Errorf("missing user message, got:\n%s", got)
	}
	if !strings.Contains(got, "[assistant]\nhi there") {
		t.Errorf("missing assistant message, got:\n%s", got)
	}
}

func TestExtractSession_CCFormat(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"hello cc"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"hi from cc"}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "[user]\nhello cc") {
		t.Errorf("missing user message, got:\n%s", got)
	}
	if !strings.Contains(got, "[assistant]\nhi from cc") {
		t.Errorf("missing assistant message, got:\n%s", got)
	}
}

func TestExtractSession_ContentBlocks(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"block content"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"some reasoning"},{"type":"text","text":"visible answer"}]}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "block content") {
		t.Errorf("missing block content, got:\n%s", got)
	}
	if !strings.Contains(got, "visible answer") {
		t.Errorf("missing visible answer, got:\n%s", got)
	}
	if !strings.Contains(got, "[thinking]") {
		t.Errorf("missing thinking block, got:\n%s", got)
	}
	if !strings.Contains(got, "some reasoning") {
		t.Errorf("missing thinking content, got:\n%s", got)
	}
}

func TestExtractSession_SkipsUnknownTypes(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"file-history-snapshot","snapshot":{}}`,
		`{"type":"thinking_level_change","thinkingLevel":"high"}`,
		`{"type":"message","message":{"role":"user","content":"real message"}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "real message") {
		t.Errorf("missing real message, got:\n%s", got)
	}
	if strings.Count(got, "[user]") != 1 {
		t.Errorf("expected 1 user marker, got %d in:\n%s", strings.Count(got, "[user]"), got)
	}
}

func TestExtractSession_MalformedJSON(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`not json at all`,
		`{"type":"message","message":{"role":"user","content":"after bad line"}}`,
		`{"truncated":`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "after bad line") {
		t.Errorf("should recover after bad JSON, got:\n%s", got)
	}
}

func TestExtractSession_NonexistentFile(t *testing.T) {
	got := extractSession("/nonexistent/path.jsonl")
	if got != "" {
		t.Errorf("expected empty string for nonexistent file, got: %q", got)
	}
}

func TestExtractSession_PiToolResults(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"message","message":{"role":"user","content":"run ls"}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"ls -la"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","content":[{"type":"text","text":"file1.txt\nfile2.txt"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc2","name":"read","arguments":{"path":"/tmp/foo"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc2","content":[{"type":"text","text":"file contents here"}]}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "[bash]\nfile1.txt") {
		t.Errorf("missing bash output, got:\n%s", got)
	}
	if !strings.Contains(got, "$ ls -la") {
		t.Errorf("missing bash command, got:\n%s", got)
	}
	if strings.Contains(got, "file contents here") {
		t.Errorf("read output should not be indexed, got:\n%s", got)
	}
}

func TestExtractSession_CCToolResults(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"grep -r TODO ."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"src/main.go:// TODO fix this"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu2","name":"Read","input":{"file_path":"/tmp/foo"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu2","content":"should not appear"}]}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "[Bash]\nsrc/main.go:// TODO fix this") {
		t.Errorf("missing Bash output, got:\n%s", got)
	}
	if !strings.Contains(got, "$ grep -r TODO .") {
		t.Errorf("missing Bash command, got:\n%s", got)
	}
	if strings.Contains(got, "should not appear") {
		t.Errorf("Read output should not be indexed, got:\n%s", got)
	}
}

func TestExtractSession_ThinkingBlocks(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me reason about this"},{"type":"text","text":"Here is my answer"}]}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "[thinking]\nLet me reason about this") {
		t.Errorf("missing thinking block, got:\n%s", got)
	}
	if !strings.Contains(got, "[assistant]\nHere is my answer") {
		t.Errorf("missing assistant text, got:\n%s", got)
	}
}

func TestExtractSession_CCThinkingBlocks(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"CC reasoning here"},{"type":"text","text":"CC answer"}]}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "[thinking]\nCC reasoning here") {
		t.Errorf("missing CC thinking block, got:\n%s", got)
	}
	if !strings.Contains(got, "[assistant]\nCC answer") {
		t.Errorf("missing CC assistant text, got:\n%s", got)
	}
}

func TestExtractSession_BinarySkipped(t *testing.T) {
	base64Data := strings.Repeat("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 3)
	path := writeTempJSONL(t, []string{
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"cat image.png"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","content":"` + base64Data + `"}}`,
	})

	got := extractSession(path)
	if strings.Contains(got, "[bash]") {
		t.Errorf("binary/base64 tool result should be skipped, got:\n%s", got)
	}
}

func TestExtractSession_CappedTier(t *testing.T) {
	var longParts []string
	for i := range 100 {
		longParts = append(longParts, fmt.Sprintf("Task result line %d: processing data", i))
	}
	longOutput := strings.Join(longParts, "\\n")
	path := writeTempJSONL(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","name":"Task","input":{"description":"sub"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"` + longOutput + `"}]}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "[Task]") {
		t.Errorf("Task output should be indexed, got:\n%s", got)
	}
	if !strings.Contains(got, "[...truncated]") {
		t.Errorf("Task output should be truncated at cap, got:\n%s", got)
	}
}

func TestExtractSession_ResultTruncation(t *testing.T) {
	var parts []string
	for i := range 200 {
		parts = append(parts, fmt.Sprintf("line %d: %s", i, strings.Repeat("data ", 20)))
	}
	longOutput := strings.Join(parts, "\\n")

	path := writeTempJSONL(t, []string{
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"big output"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","content":"` + longOutput + `"}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "[...truncated]") {
		t.Errorf("large bash output should be truncated, got length: %d", len(got))
	}
}

func TestExtractSession_UnknownToolSkipped(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"edit","arguments":{"path":"/tmp/foo"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","content":"file updated successfully"}}`,
	})

	got := extractSession(path)
	if strings.Contains(got, "file updated successfully") {
		t.Errorf("edit output should be skipped, got:\n%s", got)
	}
}

func TestTruncateResult(t *testing.T) {
	short := "hello world"
	if got := truncateResult(short, 100); got != short {
		t.Errorf("short string should be unchanged, got %q", got)
	}

	long := "line1\nline2\nline3\nline4\nline5"
	got := truncateResult(long, 18)
	if !strings.HasSuffix(got, "[...truncated]") {
		t.Errorf("should have truncated marker, got %q", got)
	}
	if strings.Contains(got, "line5") {
		t.Errorf("should not contain line5, got %q", got)
	}
}

func TestLooksLikeBinary(t *testing.T) {
	if looksLikeBinary("normal text") {
		t.Error("normal text should not be binary")
	}
	if looksLikeBinary("") {
		t.Error("empty string should not be binary")
	}

	b64 := strings.Repeat("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 3)
	if !looksLikeBinary(b64) {
		t.Error("long base64 should be detected as binary")
	}

	withNull := strings.Repeat("a", 100) + "\x00" + strings.Repeat("b", 100)
	if !looksLikeBinary(withNull) {
		t.Error("string with null bytes should be detected as binary")
	}
}

func TestGetToolTier(t *testing.T) {
	if getToolTier("bash") != tierIndex {
		t.Error("bash should be tier 1")
	}
	if getToolTier("exec_command") != tierCapped {
		t.Error("exec_command should be tier 2")
	}
	if getToolTier("Task") != tierCapped {
		t.Error("Task should be tier 2")
	}
	if getToolTier("Read") != tierSkip {
		t.Error("Read should be tier 3 (skip)")
	}
	if getToolTier("unknown_tool") != tierSkip {
		t.Error("unknown tools should default to skip")
	}
}

func TestExtractSession_CodexFormat(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello codex"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi from codex"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"internal prompt"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","call_id":"call1","name":"exec_command","arguments":"{\"cmd\":\"ls -la\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call1","output":"Command: /bin/zsh -lc \"ls -la\"\nChunk ID: abc\nWall time: 0.0 seconds\nProcess exited with code 0\nOutput:\nfile1\nfile2"}}`,
	})

	got := extractSession(path)
	if !strings.Contains(got, "[user]\nhello codex") {
		t.Errorf("missing codex user message, got:\n%s", got)
	}
	if !strings.Contains(got, "[assistant]\nhi from codex") {
		t.Errorf("missing codex assistant message, got:\n%s", got)
	}
	if strings.Contains(got, "internal prompt") {
		t.Errorf("developer content should be skipped, got:\n%s", got)
	}
	if !strings.Contains(got, "$ ls -la") {
		t.Errorf("missing codex command input, got:\n%s", got)
	}
	if !strings.Contains(got, "[exec_command]\nfile1") {
		t.Errorf("missing codex command output, got:\n%s", got)
	}
	if strings.Contains(got, "Chunk ID:") {
		t.Errorf("codex command metadata should be stripped, got:\n%s", got)
	}
}

func TestExtractSession_CodexUnknownToolSkipped(t *testing.T) {
	path := writeTempJSONL(t, []string{
		`{"type":"response_item","payload":{"type":"function_call","call_id":"call2","name":"mcp__codex_apps__github_fetch","arguments":"{\"url\":\"https://example.com\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call2","output":"sensitive payload"}}`,
	})

	got := extractSession(path)
	if strings.Contains(got, "sensitive payload") {
		t.Errorf("unknown Codex tool output should be skipped, got:\n%s", got)
	}
}

func TestBuildMatcher_Literal(t *testing.T) {
	m := buildMatcher([]string{"hello"})
	if !m("say hello world") {
		t.Error("should match 'hello' in 'say hello world'")
	}
	if !m("HELLO THERE") {
		t.Error("should match case-insensitively")
	}
	if m("no match here") {
		t.Error("should not match unrelated text")
	}
}

func TestBuildMatcher_MultipleQueries(t *testing.T) {
	m := buildMatcher([]string{"alpha", "beta"})
	if !m("has alpha") {
		t.Error("should match first query")
	}
	if !m("has beta") {
		t.Error("should match second query")
	}
	if m("has gamma") {
		t.Error("should not match unrelated text")
	}
}

func TestBuildMatcher_Regex(t *testing.T) {
	m := buildMatcher([]string{`AGDX-\d+`})
	if !m("Working on AGDX-42 today") {
		t.Error("should match regex pattern")
	}
	if !m("issue agdx-100 is fixed") {
		t.Error("should match case-insensitively")
	}
	if m("no ticket here") {
		t.Error("should not match unrelated text")
	}
}

func TestBuildMatcher_InvalidRegexFallsBackToLiteral(t *testing.T) {
	m := buildMatcher([]string{`[invalid`})
	if !m("has [invalid in it") {
		t.Error("invalid regex should fall back to literal match")
	}
}

func TestBuildAndSearchIndex(t *testing.T) {
	home := t.TempDir()
	homeDir = home

	sessDir := filepath.Join(home, ".pi", "agent", "sessions", "--test--")
	if err := os.MkdirAll(sessDir, 0o750); err != nil {
		t.Fatal(err)
	}

	session1 := filepath.Join(sessDir, "session1.jsonl")
	if err := os.WriteFile(session1, []byte(
		`{"type":"message","message":{"role":"user","content":"discuss CameraX migration"}}`+"\n"+
			`{"type":"message","message":{"role":"assistant","content":"Let me help with the CameraX migration plan"}}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	session2 := filepath.Join(sessDir, "session2.jsonl")
	if err := os.WriteFile(session2, []byte(
		`{"type":"message","message":{"role":"user","content":"fix the kotlin build"}}`+"\n"+
			`{"type":"message","message":{"role":"assistant","content":"I see the Gradle error"}}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := buildIndex(false)
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 sessions indexed, got %d", n)
	}

	idxData, err := os.ReadFile(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	idx := string(idxData)
	if !strings.Contains(idx, sessionPrefix) {
		t.Error("index should contain session markers")
	}
	if !strings.Contains(idx, "CameraX migration") {
		t.Error("index should contain session content")
	}

	n2, err := buildIndex(false)
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	if n2 != 0 {
		t.Errorf("incremental with no new files should index 0, got %d", n2)
	}

	session3 := filepath.Join(sessDir, "session3.jsonl")
	if err := os.WriteFile(session3, []byte(
		`{"type":"message","message":{"role":"user","content":"new session content"}}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	n3, err := buildIndex(false)
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	if n3 != 1 {
		t.Errorf("incremental should index 1 new session, got %d", n3)
	}

	n4, err := buildIndex(true)
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	if n4 != 3 {
		t.Errorf("rebuild should index all 3 sessions, got %d", n4)
	}
}

func TestSessionSources_IncludesCodex(t *testing.T) {
	homeDir = "/tmp/test-home"
	sources := sessionSources()
	want := filepath.Join(homeDir, ".codex", "sessions")
	found := false
	for _, src := range sources {
		if src.dir == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected session source %q in %v", want, sources)
	}
}

func TestGetAllSessionFiles_SortsAndExcludesSubagents(t *testing.T) {
	home := t.TempDir()
	homeDir = home

	piDir := filepath.Join(home, ".pi", "agent", "sessions", "--pi--")
	claudeDir := filepath.Join(home, ".claude", "projects", "project-a")
	subagentDir := filepath.Join(home, ".claude", "projects", "subagents", "worker-1")
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "04", "09")

	for _, dir := range []string{piDir, claudeDir, subagentDir, codexDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	wantPi := filepath.Join(piDir, "b.jsonl")
	wantClaude := filepath.Join(claudeDir, "a.jsonl")
	wantCodex := filepath.Join(codexDir, "c.jsonl")
	skipSubagent := filepath.Join(subagentDir, "skip.jsonl")

	for _, path := range []string{wantPi, wantClaude, wantCodex, skipSubagent} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "notes.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := getAllSessionFiles()
	want := []string{wantClaude, wantCodex, wantPi}
	if len(got) != len(want) {
		t.Fatalf("expected %d session files, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected file order/content at %d: got %q want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	for _, path := range got {
		if path == skipSubagent {
			t.Fatalf("subagent session should be excluded: %v", got)
		}
	}
}

func TestGetIndexedFiles(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "index.txt")

	got := getIndexedFiles(idxPath)
	if len(got) != 0 {
		t.Errorf("expected 0 indexed files for missing index, got %d", len(got))
	}

	content := sessionPrefix + "/path/to/session1.jsonl\n[user]\nhello\n\n" +
		sessionPrefix + "/path/to/session2.jsonl\n[assistant]\nworld\n\n"
	if err := os.WriteFile(idxPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got = getIndexedFiles(idxPath)
	if len(got) != 2 {
		t.Errorf("expected 2 indexed files, got %d", len(got))
	}
	if !got["/path/to/session1.jsonl"] || !got["/path/to/session2.jsonl"] {
		t.Errorf("wrong indexed files: %v", got)
	}
}

func TestSearchWithContext(t *testing.T) {
	lines := []string{
		sessionPrefix + "/path/session.jsonl",
		"[user]",
		"line 1",
		"line 2",
		"target match here",
		"line 4",
		"line 5",
		"line 6",
		"line 7",
	}

	m := buildMatcher([]string{"target match"})

	output := captureStdout(t, func() {
		searchWithContext(lines, m, 0)
	})

	if !strings.Contains(output, "target match here") {
		t.Errorf("output should contain match line, got:\n%s", output)
	}
	if !strings.Contains(output, "line 2") {
		t.Errorf("output should contain context before match, got:\n%s", output)
	}
	if !strings.Contains(output, "line 6") {
		t.Errorf("output should contain context after match (up to 3 lines), got:\n%s", output)
	}
	if strings.Contains(output, "line 7") {
		t.Errorf("output should NOT contain line 7 (4 lines after match, beyond context), got:\n%s", output)
	}
}

func TestSearchWithContext_NoMatches(t *testing.T) {
	lines := []string{"no match here"}
	m := buildMatcher([]string{"zzzzz"})

	output := captureStderr(t, func() {
		searchWithContext(lines, m, 0)
	})

	if !strings.Contains(output, "No matches found") {
		t.Errorf("should print no matches message, got: %q", output)
	}
}

func TestSearchJSON(t *testing.T) {
	lines := []string{
		sessionPrefix + "/path/s1.jsonl",
		"[user]",
		"find this alpha",
		"[assistant]",
		"response text",
		sessionPrefix + "/path/s2.jsonl",
		"[user]",
		"another alpha here",
	}

	m := buildMatcher([]string{"alpha"})

	output := captureStdout(t, func() {
		if err := searchJSON(lines, m, 0); err != nil {
			t.Fatalf("searchJSON: %v", err)
		}
	})

	var results []jsonResult
	if err := json.Unmarshal([]byte(output), &results); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, output)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 session groups, got %d", len(results))
	}
	if results[0].Session != "/path/s1.jsonl" {
		t.Errorf("wrong session: %s", results[0].Session)
	}
	if len(results[0].Matches) != 1 || !strings.Contains(results[0].Matches[0], "alpha") {
		t.Errorf("wrong matches: %v", results[0].Matches)
	}
}

func TestSearchJSON_Empty(t *testing.T) {
	lines := []string{"no match"}
	m := buildMatcher([]string{"zzz"})

	output := captureStdout(t, func() {
		if err := searchJSON(lines, m, 0); err != nil {
			t.Fatalf("searchJSON: %v", err)
		}
	})

	var results []jsonResult
	if err := json.Unmarshal([]byte(output), &results); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty array, got %d results", len(results))
	}
}

func TestRun_UnknownFlag(t *testing.T) {
	err := run([]string{"--wat"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_Help(t *testing.T) {
	output := captureStdout(t, func() {
		if err := run([]string{"--help"}); err != nil {
			t.Fatalf("run(--help): %v", err)
		}
	})

	if !strings.HasPrefix(output, "session-search dev\n\n") {
		t.Fatalf("help output should start with version banner:\n%s", output)
	}
	if !strings.Contains(output, "search past pi, Claude Code, and Codex sessions") {
		t.Fatalf("help output missing banner:\n%s", output)
	}
	if !strings.Contains(output, "--rebuild") {
		t.Fatalf("help output missing flags:\n%s", output)
	}
}

func TestRun_VersionFlags(t *testing.T) {
	flags := []string{"-v", "--v", "-version", "--version"}
	for _, flag := range flags {
		t.Run(flag, func(t *testing.T) {
			output := captureStdout(t, func() {
				if err := run([]string{flag}); err != nil {
					t.Fatalf("run(%s): %v", flag, err)
				}
			})

			if output != "session-search dev\n" {
				t.Fatalf("unexpected version output for %s: %q", flag, output)
			}
		})
	}
}

func TestRun_StatsWithoutIndex(t *testing.T) {
	home := t.TempDir()
	homeDir = home

	output := captureStdout(t, func() {
		if err := run([]string{"--stats"}); err != nil {
			t.Fatalf("run(--stats): %v", err)
		}
	})

	if !strings.Contains(output, "No index file found.") {
		t.Fatalf("unexpected stats output without index:\n%s", output)
	}
}

func TestSearchMaxResults(t *testing.T) {
	lines := []string{
		sessionPrefix + "/path/s.jsonl",
		"match one",
		"match two",
		"match three",
		"match four",
	}

	m := buildMatcher([]string{"match"})

	output := captureStdout(t, func() {
		if err := searchJSON(lines, m, 2); err != nil {
			t.Fatalf("searchJSON: %v", err)
		}
	})

	var results []jsonResult
	if err := json.Unmarshal([]byte(output), &results); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	total := 0
	for _, r := range results {
		total += len(r.Matches)
	}
	if total != 2 {
		t.Errorf("expected 2 matches with --max 2, got %d", total)
	}
}

func TestSearchGrouped(t *testing.T) {
	lines := []string{
		sessionPrefix + "/path/s1.jsonl",
		"find alpha here",
		"also alpha there",
		sessionPrefix + "/path/s2.jsonl",
		"no match",
		sessionPrefix + "/path/s3.jsonl",
		"alpha again",
	}

	m := buildMatcher([]string{"alpha"})

	output := captureStdout(t, func() {
		searchGrouped(lines, m, 0)
	})

	if !strings.Contains(output, "=== /path/s1.jsonl ===") {
		t.Errorf("should have s1 header, got:\n%s", output)
	}
	if !strings.Contains(output, "=== /path/s3.jsonl ===") {
		t.Errorf("should have s3 header, got:\n%s", output)
	}
	if strings.Contains(output, "=== /path/s2.jsonl ===") {
		t.Errorf("should NOT have s2 header (no matches), got:\n%s", output)
	}
}

func TestWithIndexLock_ConcurrentBuildIndex_NoDuplicates(t *testing.T) {
	home := t.TempDir()
	homeDir = home

	sessDir := filepath.Join(home, ".pi", "agent", "sessions", "--test--")
	if err := os.MkdirAll(sessDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		path := filepath.Join(sessDir, "session"+strconv.Itoa(i)+".jsonl")
		content := `{"type":"message","message":{"role":"user","content":"session ` + strconv.Itoa(i) + `"}}` + "\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if err := withIndexLock(func() error {
				_, err := buildIndex(false)
				return err
			}); err != nil {
				t.Errorf("withIndexLock: %v", err)
			}
		})
	}
	wg.Wait()

	indexed := getIndexedFiles(indexPath())
	if len(indexed) != 10 {
		t.Errorf("expected 10 indexed sessions, got %d", len(indexed))
	}

	data, err := os.ReadFile(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	markerCount := strings.Count(string(data), sessionPrefix)
	if markerCount != 10 {
		t.Errorf("expected 10 session markers in index, got %d (duplicates present)", markerCount)
	}
}

func BenchmarkSearch_Literal(b *testing.B) {
	var sb strings.Builder
	for i := range 1000 {
		sb.WriteString(sessionPrefix + "/path/session" + strconv.Itoa(i) + ".jsonl\n")
		sb.WriteString("[user]\n")
		sb.WriteString("Tell me about the CameraX migration and how to handle the Kotlin coroutines\n")
		sb.WriteString("\n[assistant]\n")
		sb.WriteString("Let me help you with the CameraX migration. First we need to update the build.gradle dependencies.\n")
		sb.WriteString("\n")
	}
	lines := strings.Split(sb.String(), "\n")
	m := buildMatcher([]string{"camerax"})

	b.ResetTimer()
	for range b.N {
		count := 0
		for _, line := range lines {
			if m(line) {
				count++
			}
		}
	}
}

func BenchmarkSearch_Regex(b *testing.B) {
	var sb strings.Builder
	for i := range 1000 {
		sb.WriteString(sessionPrefix + "/path/session" + strconv.Itoa(i) + ".jsonl\n")
		sb.WriteString("[user]\n")
		sb.WriteString("Working on AGDX-42 and also AGDX-100 today\n")
		sb.WriteString("\n[assistant]\n")
		sb.WriteString("I see those tickets. Let me check AGDX-42 first.\n")
		sb.WriteString("\n")
	}
	lines := strings.Split(sb.String(), "\n")
	m := buildMatcher([]string{`AGDX-\d+`})

	b.ResetTimer()
	for range b.N {
		count := 0
		for _, line := range lines {
			if m(line) {
				count++
			}
		}
	}
}

func BenchmarkSearch_MultiQuery(b *testing.B) {
	var sb strings.Builder
	for i := range 1000 {
		sb.WriteString(sessionPrefix + "/path/session" + strconv.Itoa(i) + ".jsonl\n")
		sb.WriteString("[user]\n")
		sb.WriteString("Discuss the camera migration and kotlin coroutines in the android project\n")
		sb.WriteString("\n[assistant]\n")
		sb.WriteString("The migration involves updating several gradle dependencies and refactoring.\n")
		sb.WriteString("\n")
	}
	lines := strings.Split(sb.String(), "\n")
	m := buildMatcher([]string{"camera", "kotlin", "gradle"})

	b.ResetTimer()
	for range b.N {
		count := 0
		for _, line := range lines {
			if m(line) {
				count++
			}
		}
	}
}
