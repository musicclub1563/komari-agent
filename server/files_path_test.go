package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	v2 "github.com/komari-monitor/komari-agent/protocol/v2"
)

// TestResolveFilePathPreservesWhitespace pins the resolver contract: it must
// never rewrite the bytes of a path that came over RPC.
func TestResolveFilePathPreservesWhitespace(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "report")
	withTrailingSpace := filepath.Join(root, "report ")

	if got, want := resolveFilePath(plain), plain; got != want {
		t.Fatalf("resolveFilePath(%q) = %q, want %q", plain, got, want)
	}
	// filepath.Clean only touches separators and dot elements; spaces survive.
	if got, want := resolveFilePath(withTrailingSpace), withTrailingSpace; got != want {
		t.Fatalf("resolveFilePath(%q) = %q, want %q", withTrailingSpace, got, want)
	}
	if resolveFilePath(plain) == resolveFilePath(withTrailingSpace) {
		t.Fatal("resolver collapsed a trailing-space name onto its neighbour")
	}

	// The empty-path fallback must still resolve to the filesystem root.
	if got := resolveFilePath(""); got != resolveOSPath("/") {
		t.Fatalf("resolveFilePath(\"\") = %q, want %q", got, resolveOSPath("/"))
	}
}

// TestDeletePathKeepsTrailingSpaceNameExact reproduces the reported defect:
// deleting "report " used to remove "report" instead.
func TestDeletePathKeepsTrailingSpaceNameExact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Win32 strips trailing spaces from path names, so the name cannot exist here")
	}
	root := t.TempDir()
	target := filepath.Join(root, "report ")
	neighbour := filepath.Join(root, "report")
	writeFile(t, target, "target")
	writeFile(t, neighbour, "neighbour")

	if _, err := deletePath(target); err != nil {
		t.Fatalf("deletePath(%q): %v", target, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("target %q still exists (err=%v)", target, err)
	}
	assertFileContent(t, neighbour, "neighbour")
}

// TestDeletePathPreservesNeighbouringFiles covers the same class of bug with
// names that are legal on every platform: inner spaces, CJK and near-duplicates.
func TestDeletePathPreservesNeighbouringFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"report":            "plain",
		"report final.txt":  "inner space",
		"报告.txt":            "cjk",
		"repo rt":           "space split",
		"report-final.txt":  "near duplicate",
	}
	for name, content := range files {
		writeFile(t, filepath.Join(root, name), content)
	}

	victim := filepath.Join(root, "report final.txt")
	if _, err := deletePath(victim); err != nil {
		t.Fatalf("deletePath(%q): %v", victim, err)
	}
	if _, err := os.Lstat(victim); !os.IsNotExist(err) {
		t.Fatalf("victim %q still exists (err=%v)", victim, err)
	}
	for name, content := range files {
		if name == "report final.txt" {
			continue
		}
		assertFileContent(t, filepath.Join(root, name), content)
	}
}

// TestFileOperationsPreserveSpacePaths walks the operations that receive a path
// over RPC and verifies they act on exactly that entry.
func TestFileOperationsPreserveSpacePaths(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "data set.txt")
	writeFile(t, source, "payload")

	statted := mustRunFileOperation(t, v2.FileOperation{
		Op:   "stat",
		Args: map[string]interface{}{"path": source},
	})
	var info fileInfo
	if err := json.Unmarshal(statted, &info); err != nil {
		t.Fatalf("decode stat result: %v", err)
	}
	if info.Path != virtualizeFilePath(source) || info.Size != int64(len("payload")) {
		t.Fatalf("stat returned %+v, want path %q size 7", info, virtualizeFilePath(source))
	}

	destination := filepath.Join(root, "data set copy.txt")
	mustRunFileOperation(t, v2.FileOperation{
		Op:   "copy",
		Args: map[string]interface{}{"source": source, "destination": destination},
	})
	assertFileContent(t, destination, "payload")
	assertFileContent(t, source, "payload")

	renamed := filepath.Join(root, "moved set.txt")
	mustRunFileOperation(t, v2.FileOperation{
		Op:   "move",
		Args: map[string]interface{}{"source": destination, "destination": renamed},
	})
	assertFileContent(t, renamed, "payload")

	mustRunFileOperation(t, v2.FileOperation{
		Op:   "chmod",
		Args: map[string]interface{}{"path": renamed, "mode": "0644"},
	})
	assertFileContent(t, renamed, "payload")

	// Nothing may have leaked into the neighbouring plain name.
	if _, err := os.Lstat(filepath.Join(root, "data set")); !os.IsNotExist(err) {
		t.Fatalf("unexpected sibling entry: %v", err)
	}
}

// TestMkdirKeepsTrailingSpaceNameExact guards the same normalisation on the
// directory-creation path.
func TestMkdirKeepsTrailingSpaceNameExact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Win32 strips trailing spaces from path names, so the name cannot exist here")
	}
	root := t.TempDir()
	withSpace := filepath.Join(root, "logs ")
	plain := filepath.Join(root, "logs")

	if _, err := mkdir(withSpace, "0755"); err != nil {
		t.Fatalf("mkdir(%q): %v", withSpace, err)
	}
	if info, err := os.Stat(withSpace); err != nil || !info.IsDir() {
		t.Fatalf("directory %q missing: %v", withSpace, err)
	}
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Fatalf("neighbouring directory %q was created (err=%v)", plain, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if string(content) != want {
		t.Fatalf("content of %q = %q, want %q", path, content, want)
	}
}
