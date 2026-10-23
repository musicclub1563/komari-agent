package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func seedUploadSession(t *testing.T, dir, uploadID, name string, lastActivity time.Time) string {
	t.Helper()
	partPath := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(partPath, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write part: %v", err)
	}
	uploadChunksMu.Lock()
	uploadChunks[uploadID] = uploadChunkState{
		ExpectedSize: 7,
		ChunkSize:    defaultTransferChunkSize,
		TargetPath:   filepath.Join(dir, "target.bin"),
		Parts:        map[int64]struct{}{0: {}},
		PartCount:    1,
		TempPath:     partPath,
		CreatedAt:    lastActivity,
		LastActivity: lastActivity,
	}
	uploadChunksMu.Unlock()
	return partPath
}

func TestReapIdleUploadSessionRemovesPartStateAndLock(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	now := time.Now()
	partPath := seedUploadSession(t, dir, "idle-upload", "part-idle", now.Add(-time.Hour))

	if removed := reapIdleUploadSessions(now, uploadIdleSessionTTL); removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf("part file still present, stat err = %v", err)
	}
	uploadChunksMu.Lock()
	_, exists := uploadChunks["idle-upload"]
	uploadChunksMu.Unlock()
	if exists {
		t.Fatal("upload state still registered")
	}
	uploadWriteLocksMu.Lock()
	_, lockExists := uploadWriteLocks["idle-upload"]
	uploadWriteLocksMu.Unlock()
	if lockExists {
		t.Fatal("upload write lock entry leaked after reap")
	}
}

func TestReapKeepsUploadWithActiveStream(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	now := time.Now()
	partPath := seedUploadSession(t, dir, "live-upload", "part-live", now.Add(-time.Hour))

	_, cancel := context.WithCancel(context.Background())
	endActive, err := beginUploadStream("live-upload", "transfer-1", cancel)
	if err != nil {
		t.Fatalf("beginUploadStream: %v", err)
	}

	if removed := reapIdleUploadSessions(now, uploadIdleSessionTTL); removed != 0 {
		t.Fatalf("removed = %d, want 0 while a stream is in flight", removed)
	}
	if _, err := os.Stat(partPath); err != nil {
		t.Fatalf("part file removed while streaming: %v", err)
	}
	uploadChunksMu.Lock()
	_, exists := uploadChunks["live-upload"]
	uploadChunksMu.Unlock()
	if !exists {
		t.Fatal("upload state removed while streaming")
	}

	endActive()
	cancel()
	if removed := reapIdleUploadSessions(now, uploadIdleSessionTTL); removed != 1 {
		t.Fatalf("removed after stream ended = %d, want 1", removed)
	}
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf("part file survived reap, stat err = %v", err)
	}
}

func TestReapKeepsRecentlyActiveUpload(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	now := time.Now()
	partPath := seedUploadSession(t, dir, "fresh-upload", "part-fresh", now.Add(-time.Minute))

	if removed := reapIdleUploadSessions(now, uploadIdleSessionTTL); removed != 0 {
		t.Fatalf("removed = %d, want 0 for a session that just received a chunk", removed)
	}
	if _, err := os.Stat(partPath); err != nil {
		t.Fatalf("fresh part file removed: %v", err)
	}
}

func TestReapSkipsSessionHeldByCommit(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	now := time.Now()
	partPath := seedUploadSession(t, dir, "committing-upload", "part-committing", now.Add(-time.Hour))

	guard := acquireUploadWriteLock("committing-upload")
	removed := reapIdleUploadSessions(now, uploadIdleSessionTTL)
	guard.release()
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 while upload_commit holds the session", removed)
	}
	if _, err := os.Stat(partPath); err != nil {
		t.Fatalf("part file removed during commit: %v", err)
	}
}

func TestReapHonoursTTLBoundary(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	now := time.Now()
	seedUploadSession(t, dir, "boundary-upload", "part-boundary", now.Add(-10*time.Minute))

	if removed := reapIdleUploadSessions(now, 11*time.Minute); removed != 0 {
		t.Fatalf("removed below ttl = %d, want 0", removed)
	}
	if removed := reapIdleUploadSessions(now, 10*time.Minute); removed != 1 {
		t.Fatalf("removed at ttl = %d, want 1", removed)
	}
}

func TestWriteUploadStreamChunkRefreshesLastActivity(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "chunked.bin")

	firstSpec := uploadStreamSpec{
		Path: target, UploadID: "activity-upload",
		ChunkSize: 10, TotalSize: 20, ChunkCount: 2,
		ChunkIndex: 0, Offset: 0, Expected: 10, First: true,
	}
	if _, err := writeUploadStreamChunk(firstSpec, strings.NewReader("0123456789")); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	uploadChunksMu.Lock()
	first := uploadChunks["activity-upload"]
	uploadChunksMu.Unlock()
	if first.LastActivity.IsZero() {
		t.Fatal("LastActivity not recorded for the first chunk")
	}
	if first.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not recorded for the first chunk")
	}

	time.Sleep(10 * time.Millisecond)
	secondSpec := firstSpec
	secondSpec.ChunkIndex = 1
	secondSpec.Offset = 10
	secondSpec.First = false
	if _, err := writeUploadStreamChunk(secondSpec, strings.NewReader("abcdefghij")); err != nil {
		t.Fatalf("second chunk: %v", err)
	}
	uploadChunksMu.Lock()
	second := uploadChunks["activity-upload"]
	uploadChunksMu.Unlock()
	if !second.LastActivity.After(first.LastActivity) {
		t.Fatalf("LastActivity not refreshed: %v -> %v", first.LastActivity, second.LastActivity)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt must stay the creation time: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
}

func TestRemoveStaleUploadParts(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	now := time.Now()
	target := filepath.Join(dir, "target.bin")
	old := now.Add(-time.Hour)

	stale := filepath.Join(dir, ".target.bin.komari-upload-old.part")
	fresh := filepath.Join(dir, ".target.bin.komari-upload-fresh.part")
	live := filepath.Join(dir, ".target.bin.komari-upload-live.part")
	unrelated := filepath.Join(dir, ".other.bin.komari-upload-old.part")
	for _, path := range []string{stale, fresh, live, unrelated} {
		if err := os.WriteFile(path, []byte("part"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	for _, path := range []string{stale, live, unrelated} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	uploadChunksMu.Lock()
	uploadChunks["live"] = uploadChunkState{TargetPath: target, TempPath: live}
	uploadChunksMu.Unlock()

	removeStaleUploadParts(target, "", now)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale part survived, stat err = %v", err)
	}
	for _, path := range []string{fresh, live, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s should have been kept: %v", path, err)
		}
	}
}

// TestUploadWriteLockNeverSplitsIntoTwoLocks pins the reference-counting
// contract: while a goroutine waits for the per-session lock, the entry must
// stay in the map, otherwise a third caller would create a second mutex for the
// same upload_id and run concurrently with the waiter.
func TestUploadWriteLockNeverSplitsIntoTwoLocks(t *testing.T) {
	resetUploadStreamState(t)
	const uploadID = "lock-upload"

	first := acquireUploadWriteLock(uploadID)

	acquired := make(chan *uploadWriteGuard, 1)
	go func() {
		acquired <- acquireUploadWriteLock(uploadID)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		uploadWriteLocksMu.Lock()
		entry := uploadWriteLocks[uploadID]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		uploadWriteLocksMu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiter did not register on the lock entry, refs = %d", refs)
		}
		time.Sleep(time.Millisecond)
	}

	first.release()

	uploadWriteLocksMu.Lock()
	entry := uploadWriteLocks[uploadID]
	uploadWriteLocksMu.Unlock()
	if entry == nil {
		t.Fatal("lock entry removed while a goroutine was still waiting on it")
	}

	second := <-acquired
	if second.entry != first.entry {
		t.Fatal("waiter received a different mutex for the same upload_id")
	}
	second.release()

	uploadWriteLocksMu.Lock()
	_, leaked := uploadWriteLocks[uploadID]
	uploadWriteLocksMu.Unlock()
	if leaked {
		t.Fatal("lock entry leaked after the last release")
	}
}

// TestReapNeverDropsPartOfRegisteredStream races the reaper against stream
// registration. A registered stream must either see its session and part file
// intact or see neither: the reaper may not delete state under an active
// stream, and it may not unlink a part file that a stream is about to write.
func TestReapNeverDropsPartOfRegisteredStream(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	const uploadID = "race-upload"
	seedUploadSession(t, dir, uploadID, "part-race", time.Now().Add(-time.Hour))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var violations atomic.Int64

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			reapIdleUploadSessions(time.Now(), uploadIdleSessionTTL)
		}
	}()

	for i := 0; i < 300; i++ {
		_, cancel := context.WithCancel(context.Background())
		endActive, err := beginUploadStream(uploadID, fmt.Sprintf("stream-%d", i), cancel)
		if err != nil {
			cancel()
			continue
		}

		uploadChunksMu.Lock()
		state, exists := uploadChunks[uploadID]
		uploadChunksMu.Unlock()
		if exists {
			if _, statErr := os.Stat(state.TempPath); statErr != nil {
				violations.Add(1)
			}
		} else {
			// The previous pass reclaimed the abandoned session before this
			// stream registered; recreate it so the race keeps going.
			seedUploadSession(t, dir, uploadID, "part-race", time.Now().Add(-time.Hour))
		}

		endActive()
		cancel()
	}
	close(stop)
	wg.Wait()

	if count := violations.Load(); count != 0 {
		t.Fatalf("observed %d reap(s) that removed a part file under an active stream", count)
	}
}

// TestReclaimHoldsLifecycleLockAgainstRegistration proves deterministically
// that the idle check and the unlink happen inside the same critical section
// that beginUploadStream uses. With the check and the delete split across two
// smaller sections, a stream could register in between and start writing to a
// part file that is about to be removed.
func TestReclaimHoldsLifecycleLockAgainstRegistration(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	now := time.Now()
	const uploadID = "locked-upload"
	seedUploadSession(t, dir, uploadID, "part-locked", now.Add(-time.Hour))

	entered := make(chan struct{})
	unblock := make(chan struct{})
	uploadReclaimTestHook = func() {
		close(entered)
		<-unblock
	}
	defer func() { uploadReclaimTestHook = nil }()

	reaped := make(chan int, 1)
	go func() { reaped <- reapIdleUploadSessions(now, uploadIdleSessionTTL) }()
	<-entered

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	registered := make(chan error, 1)
	go func() {
		endActive, err := beginUploadStream(uploadID, "late-stream", cancel)
		if err == nil {
			endActive()
		}
		registered <- err
	}()

	select {
	case <-registered:
		t.Fatal("beginUploadStream completed while reclamation held the lifecycle lock")
	case <-time.After(200 * time.Millisecond):
	}

	close(unblock)
	if removed := <-reaped; removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("beginUploadStream never completed after reclamation released the lock")
	}
}

func TestLateChunkAfterReapRequiresFirst(t *testing.T) {
	resetUploadStreamState(t)
	dir := t.TempDir()
	now := time.Now()
	target := filepath.Join(dir, "late.bin")
	seedUploadSession(t, dir, "late-upload", "part-late", now.Add(-time.Hour))
	if removed := reapIdleUploadSessions(now, uploadIdleSessionTTL); removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	continuation := uploadStreamSpec{
		Path: target, UploadID: "late-upload",
		ChunkSize: 4, TotalSize: 8, ChunkCount: 2,
		ChunkIndex: 1, Offset: 4, Expected: 4, First: false,
	}
	if _, err := writeUploadStreamChunk(continuation, strings.NewReader("ijkl")); err == nil {
		t.Fatal("late continuation chunk must be rejected once the session was reclaimed")
	}

	restart := continuation
	restart.ChunkIndex = 0
	restart.Offset = 0
	restart.First = true
	if _, err := writeUploadStreamChunk(restart, strings.NewReader("abcd")); err != nil {
		t.Fatalf("restart with first=true: %v", err)
	}
}

func TestUploadSessionCapacityLimit(t *testing.T) {
	resetUploadStreamState(t)
	uploadChunksMu.Lock()
	for i := 0; i < maxUploadSessions; i++ {
		uploadChunks[fmt.Sprintf("filler-%d", i)] = uploadChunkState{}
	}
	uploadChunksMu.Unlock()

	spec := uploadStreamSpec{
		Path: filepath.Join(t.TempDir(), "overflow.bin"), UploadID: "overflow",
		ChunkSize: 4, TotalSize: 4, ChunkCount: 1,
		ChunkIndex: 0, Offset: 0, Expected: 4, First: true,
	}
	_, err := writeUploadStreamChunk(spec, strings.NewReader("abcd"))
	if err == nil || !strings.Contains(err.Error(), "too many concurrent upload sessions") {
		t.Fatalf("err = %v, want capacity rejection", err)
	}
}

// TestReapRetainsStateWhenPartRemovalFails covers the Windows share-violation
// case: an open handle makes the unlink fail, and the session must survive so
// the next pass can retry instead of leaking the part file silently.
func TestReapRetainsStateWhenPartRemovalFails(t *testing.T) {
	if os.PathSeparator != '\\' {
		t.Skip("unlink of an open file only fails on Windows")
	}
	resetUploadStreamState(t)
	dir := t.TempDir()
	now := time.Now()
	partPath := seedUploadSession(t, dir, "busy-upload", "part-busy", now.Add(-time.Hour))

	handle, err := os.Open(partPath)
	if err != nil {
		t.Fatalf("open part: %v", err)
	}
	defer handle.Close()

	if removed := reapIdleUploadSessions(now, uploadIdleSessionTTL); removed != 0 {
		t.Fatalf("removed = %d, want 0 while the part file is held open", removed)
	}
	uploadChunksMu.Lock()
	state, exists := uploadChunks["busy-upload"]
	uploadChunksMu.Unlock()
	if !exists || state.TempPath != partPath {
		t.Fatal("session state must be retained so the next pass can retry the unlink")
	}
}
