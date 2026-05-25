package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// withDevNullStdin points os.Stdin at /dev/null so the share parser sees a
// non-piped stdin (character device, not a pipe).
func withDevNullStdin(t *testing.T) {
	t.Helper()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	orig := os.Stdin
	os.Stdin = null
	t.Cleanup(func() {
		os.Stdin = orig
		null.Close()
	})
}

// withStdin makes os.Stdin a pipe pre-filled with content, simulating
// `echo content | agent-chat share ...`.
func withStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		r.Close()
	})
}

var artifactNameRe = regexp.MustCompile(`^(\d+)-([0-9a-f]{6})-(.+)$`)

func decodeLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range readLines(t, path) {
		m := map[string]any{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestShareFromFile(t *testing.T) {
	home := withTempHome(t)
	withDevNullStdin(t)

	src := filepath.Join(t.TempDir(), "foo.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := run([]string{"share", "--as", "alice", "@bob", "--file", src, "--note", "draft"}); rc != 0 {
		t.Fatalf("share rc = %d", rc)
	}

	recs := decodeLog(t, filepath.Join(home, "log.jsonl"))
	if len(recs) != 1 {
		t.Fatalf("want 1 log line, got %d", len(recs))
	}
	r := recs[0]
	if r["from"] != "alice" || r["to"] != "@bob" || r["note"] != "draft" {
		t.Errorf("bad record: %v", r)
	}
	if _, ok := r["text"]; ok {
		t.Errorf("text field should be omitted for share: %v", r)
	}
	path, _ := r["path"].(string)
	wantPrefix := filepath.Join(home, "artifacts", "alice") + string(filepath.Separator)
	if !strings.HasPrefix(path, wantPrefix) {
		t.Errorf("path %q not under %q", path, wantPrefix)
	}
	m := artifactNameRe.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		t.Fatalf("artifact name %q does not match <ms>-<hex6>-<basename>", filepath.Base(path))
	}
	if m[3] != "foo.txt" {
		t.Errorf("basename in artifact name = %q, want foo.txt", m[3])
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("artifact content = %q, want hello", got)
	}
}

func TestShareFromStdin(t *testing.T) {
	home := withTempHome(t)
	withStdin(t, "hello from stdin")

	if rc := run([]string{"share", "--as", "alice", "@bob"}); rc != 0 {
		t.Fatalf("share rc = %d", rc)
	}
	recs := decodeLog(t, filepath.Join(home, "log.jsonl"))
	if len(recs) != 1 {
		t.Fatalf("want 1, got %d", len(recs))
	}
	path := recs[0]["path"].(string)
	m := artifactNameRe.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		t.Fatalf("artifact name %q does not match", filepath.Base(path))
	}
	if m[3] != "stdin" {
		t.Errorf("basename = %q, want stdin", m[3])
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello from stdin" {
		t.Errorf("artifact content = %q", got)
	}
}

func TestShareFileAndStdinIsError(t *testing.T) {
	home := withTempHome(t)
	withStdin(t, "should not be read")
	src := filepath.Join(t.TempDir(), "foo.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"share", "--as", "alice", "@bob", "--file", src}); rc == 0 {
		t.Fatalf("expected non-zero rc when --file and stdin both present")
	}
	if _, err := os.Stat(filepath.Join(home, "log.jsonl")); !os.IsNotExist(err) {
		t.Errorf("log should not exist after parse error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "artifacts")); !os.IsNotExist(err) {
		t.Errorf("artifacts dir should not exist after parse error: %v", err)
	}
}

func TestShareRapidDoubleShareNoCollide(t *testing.T) {
	home := withTempHome(t)
	withDevNullStdin(t)

	src1 := filepath.Join(t.TempDir(), "a.txt")
	src2 := filepath.Join(t.TempDir(), "b.txt")
	if err := os.WriteFile(src1, []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src2, []byte("bbb"), 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := run([]string{"share", "--as", "alice", "@bob", "--file", src1}); rc != 0 {
		t.Fatalf("share #1 rc = %d", rc)
	}
	if rc := run([]string{"share", "--as", "alice", "@bob", "--file", src2}); rc != 0 {
		t.Fatalf("share #2 rc = %d", rc)
	}

	recs := decodeLog(t, filepath.Join(home, "log.jsonl"))
	if len(recs) != 2 {
		t.Fatalf("want 2 log lines, got %d", len(recs))
	}
	p1, p2 := recs[0]["path"].(string), recs[1]["path"].(string)
	if p1 == p2 {
		t.Errorf("artifact paths collided: %q", p1)
	}
	for _, p := range []string{p1, p2} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("artifact missing: %v", err)
		}
	}
}

func TestShareCreatesNestedArtifactDirs(t *testing.T) {
	home := withTempHome(t)
	withDevNullStdin(t)
	src := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(src, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"share", "--as", "alice", "@bob", "--file", src}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	fi, err := os.Stat(filepath.Join(home, "artifacts", "alice"))
	if err != nil {
		t.Fatalf("artifacts/alice not created: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("artifacts/alice is not a dir")
	}
}

func TestShareWirePathAlwaysUnderArtifacts(t *testing.T) {
	home := withTempHome(t)
	withDevNullStdin(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "secret.txt")
	if err := os.WriteFile(src, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"share", "--as", "alice", "@bob", "--file", src}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	recs := decodeLog(t, filepath.Join(home, "log.jsonl"))
	path := recs[0]["path"].(string)
	if strings.HasPrefix(path, srcDir) {
		t.Errorf("wire path leaks source directory: %q", path)
	}
	wantPrefix := filepath.Join(home, "artifacts") + string(filepath.Separator)
	if !strings.HasPrefix(path, wantPrefix) {
		t.Errorf("path not under artifacts/: %q", path)
	}
}

func TestShareMultiRecipientEmitsLinePerRecipient(t *testing.T) {
	home := withTempHome(t)
	withDevNullStdin(t)
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"share", "--as", "alice", "@bob", "@carol", "--file", src}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	recs := decodeLog(t, filepath.Join(home, "log.jsonl"))
	if len(recs) != 2 {
		t.Fatalf("want 2 lines, got %d", len(recs))
	}
	if recs[0]["path"].(string) != recs[1]["path"].(string) {
		t.Errorf("multi-recipient share should reference one artifact; got %v vs %v",
			recs[0]["path"], recs[1]["path"])
	}
	tos := map[string]bool{recs[0]["to"].(string): true, recs[1]["to"].(string): true}
	if !tos["@bob"] || !tos["@carol"] {
		t.Errorf("wrong recipient set: %v", tos)
	}
}

func TestShareBroadcastRecipient(t *testing.T) {
	home := withTempHome(t)
	withDevNullStdin(t)
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"share", "--as", "alice", "*", "--file", src}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	recs := decodeLog(t, filepath.Join(home, "log.jsonl"))
	if len(recs) != 1 || recs[0]["to"] != "*" {
		t.Errorf("want one broadcast line, got %v", recs)
	}
}

func TestShareRejectsBadInvocations(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no resolvable nick", []string{"share", "@bob", "--file", "/tmp/x"}},
		{"missing recipient", []string{"share", "--as", "alice", "--file", "/tmp/x"}},
		{"bare nick recipient", []string{"share", "--as", "alice", "bob", "--file", "/tmp/x"}},
		{"no source (no file, no piped stdin)", []string{"share", "--as", "alice", "@bob"}},
		{"unknown flag", []string{"share", "--as", "alice", "@bob", "--bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cleanResolverEnv(t)
			withDevNullStdin(t)
			if rc := run(c.args); rc == 0 {
				t.Errorf("expected non-zero rc for %q", c.name)
			}
		})
	}
}

func TestShareFlagEqualsSyntax(t *testing.T) {
	home := withTempHome(t)
	withDevNullStdin(t)
	src := filepath.Join(t.TempDir(), "z.txt")
	if err := os.WriteFile(src, []byte("zzz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"share", "--as=alice", "@bob", "--file=" + src, "--note=hi there"}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	recs := decodeLog(t, filepath.Join(home, "log.jsonl"))
	if len(recs) != 1 || recs[0]["note"] != "hi there" {
		t.Errorf("equals-syntax flags not honoured: %v", recs)
	}
}
