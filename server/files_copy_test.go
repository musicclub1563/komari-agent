package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// linkOrSkip creates a hard link and skips the test when the filesystem or the
// platform refuses it.
func linkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Link(oldname, newname); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
}

func symlinkOrSkip(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func assertNoCopyTemps(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.HasPrefix(entry.Name(), ".komari-copy-") {
			t.Fatalf("temporary copy file %q was left behind", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", dir, err)
	}
}

// TestCopyRegularFileRejectsSamePath pins the trivial alias: identical strings.
func TestCopyRegularFileRejectsSamePath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "data.txt")
	writeFile(t, path, "payload")

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyRegularFile(path, path, info); err == nil {
		t.Fatal("copy onto the same path was accepted")
	}
	assertFileContent(t, path, "payload")
	assertNoCopyTemps(t, root)
}

// TestCopyRejectsHardLinkAlias reproduces the reported data loss: two paths,
// one inode. The old implementation truncated the destination (and therefore
// the source) and still reported success.
func TestCopyRejectsHardLinkAlias(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	alias := filepath.Join(root, "alias.txt")
	writeFile(t, source, "important")
	linkOrSkip(t, source, alias)

	if _, err := copyPath(source, alias); err == nil {
		t.Fatal("copy onto a hard link of the source was accepted")
	}
	assertFileContent(t, source, "important")
	assertFileContent(t, alias, "important")
	assertNoCopyTemps(t, root)
}

// TestCopyRejectsSymlinkDestinationPointingAtSource covers the other alias
// shape: the destination path itself is a symlink that resolves to the source.
func TestCopyRejectsSymlinkDestinationPointingAtSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	link := filepath.Join(root, "link.txt")
	writeFile(t, source, "important")
	symlinkOrSkip(t, source, link)

	if _, err := copyPath(source, link); err == nil {
		t.Fatal("copy onto a symlink pointing at the source was accepted")
	}
	assertFileContent(t, source, "important")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("destination symlink was destroyed: %v", err)
	}
	if target != source {
		t.Fatalf("destination symlink retargeted to %q, want %q", target, source)
	}
	assertNoCopyTemps(t, root)
}

// TestCopySymlinkOntoItsTargetIsRejected ensures a symlink copy cannot remove
// the file it points at.
func TestCopySymlinkOntoItsTargetIsRejected(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	writeFile(t, target, "important")
	symlinkOrSkip(t, target, link)

	if _, err := copyPath(link, target); err == nil {
		t.Fatal("copy of a symlink onto its own target was accepted")
	}
	assertFileContent(t, target, "important")
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("source symlink was damaged: info=%v err=%v", info, err)
	}
}

// TestCopyRegularFileOverwritesAndPreservesMetadata keeps the intended
// semantics: replacing an existing regular file still works.
func TestCopyRegularFileOverwritesAndPreservesMetadata(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	destination := filepath.Join(root, "destination.txt")
	writeFile(t, source, "new content")
	writeFile(t, destination, "old content")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(source, 0o640); err != nil {
			t.Fatal(err)
		}
	}

	info, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyRegularFile(source, destination, info); err != nil {
		t.Fatalf("copyRegularFile: %v", err)
	}
	assertFileContent(t, destination, "new content")
	assertFileContent(t, source, "new content")
	if runtime.GOOS != "windows" {
		copied, err := os.Stat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if copied.Mode().Perm() != 0o640 {
			t.Fatalf("copied mode = %o, want 640", copied.Mode().Perm())
		}
		if !copied.ModTime().Equal(info.ModTime()) {
			t.Fatalf("copied mtime = %v, want %v", copied.ModTime(), info.ModTime())
		}
	}
	assertNoCopyTemps(t, root)
}

// TestCopyFailureLeavesDestinationIntact simulates a source that disappears
// between stat and copy: the existing destination must keep its content and no
// temporary file may be left behind.
func TestCopyFailureLeavesDestinationIntact(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	destination := filepath.Join(root, "destination.txt")
	writeFile(t, source, "gone soon")
	writeFile(t, destination, "keep me")

	info, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}

	if err := copyRegularFile(source, destination, info); err == nil {
		t.Fatal("copy of a vanished source was reported as successful")
	}
	assertFileContent(t, destination, "keep me")
	assertNoCopyTemps(t, root)
}

// TestCopyFailureIntoInvalidDestinationKeepsSourceIntact checks the destination
// side: a failing commit must not damage the source.
func TestCopyFailureIntoInvalidDestinationKeepsSourceIntact(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	writeFile(t, source, "keep me")

	blockedParent := filepath.Join(root, "blocked")
	writeFile(t, blockedParent, "i am a file")
	destination := filepath.Join(blockedParent, "nested", "destination.txt")

	if _, err := copyPath(source, destination); err == nil {
		t.Fatal("copy into a path below a regular file was accepted")
	}
	assertFileContent(t, source, "keep me")
	assertFileContent(t, blockedParent, "i am a file")
	assertNoCopyTemps(t, root)
}

// TestCopyDirectoryStillWorks is a regression guard for the directory walk that
// shares copyRegularFile.
func TestCopyDirectoryStillWorks(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "nested", "file with space.txt"), "copied")

	if _, err := copyPath(source, destination); err != nil {
		t.Fatalf("copy directory: %v", err)
	}
	assertFileContent(t, filepath.Join(destination, "nested", "file with space.txt"), "copied")
	assertNoCopyTemps(t, destination)
}
