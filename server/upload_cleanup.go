package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// uploadIdleSessionTTL is how long an upload session may go without a
	// chunk before its part file and in-memory state are reclaimed. The clock
	// is the session's own last activity, so a large upload that keeps
	// streaming stays alive however long it takes.
	uploadIdleSessionTTL = 30 * time.Minute
	// uploadReapInterval bounds how long an abandoned part file survives.
	uploadReapInterval = 5 * time.Minute
	// maxUploadSessions caps how many part files and in-memory sessions a
	// misbehaving peer can open at once. Uploads are normally serialized by the
	// caller, so this only rejects a flood of distinct upload_id values.
	maxUploadSessions = 64
)

var uploadReaperOnce sync.Once

// uploadReclaimTestHook runs inside the reclamation critical section, after the
// idle check and before the part file is unlinked. Tests use it to prove that
// stream registration is mutually exclusive with that decision; production
// leaves it nil.
var uploadReclaimTestHook func()

// uploadWriteLockEntry is a reference-counted mutex per upload session.
//
// The count exists so the map entry can never be removed while another
// goroutine still holds or waits on it: a goroutine increments refs before
// blocking on mu, so refs stays positive until every participant has finished.
// Replacing the entry would hand out a second mutex for the same upload_id and
// let commit and cancel run concurrently.
type uploadWriteLockEntry struct {
	mu   sync.Mutex
	refs int
}

type uploadWriteGuard struct {
	uploadID string
	entry    *uploadWriteLockEntry
}

// acquireUploadWriteLock takes the per-session lock, blocking while a commit,
// a cancel or the reaper owns it.
func acquireUploadWriteLock(uploadID string) *uploadWriteGuard {
	uploadWriteLocksMu.Lock()
	entry := uploadWriteLocks[uploadID]
	if entry == nil {
		entry = &uploadWriteLockEntry{}
		uploadWriteLocks[uploadID] = entry
	}
	entry.refs++
	uploadWriteLocksMu.Unlock()

	entry.mu.Lock()
	return &uploadWriteGuard{uploadID: uploadID, entry: entry}
}

// tryAcquireUploadWriteLock takes the per-session lock only when it is free.
func tryAcquireUploadWriteLock(uploadID string) *uploadWriteGuard {
	uploadWriteLocksMu.Lock()
	entry := uploadWriteLocks[uploadID]
	if entry == nil {
		entry = &uploadWriteLockEntry{}
		uploadWriteLocks[uploadID] = entry
	}
	entry.refs++
	uploadWriteLocksMu.Unlock()

	if !entry.mu.TryLock() {
		releaseUploadWriteLockRef(uploadID, entry)
		return nil
	}
	return &uploadWriteGuard{uploadID: uploadID, entry: entry}
}

func (guard *uploadWriteGuard) release() {
	if guard == nil || guard.entry == nil {
		return
	}
	guard.entry.mu.Unlock()
	releaseUploadWriteLockRef(guard.uploadID, guard.entry)
}

func releaseUploadWriteLockRef(uploadID string, entry *uploadWriteLockEntry) {
	uploadWriteLocksMu.Lock()
	entry.refs--
	if entry.refs <= 0 && uploadWriteLocks[uploadID] == entry {
		delete(uploadWriteLocks, uploadID)
	}
	uploadWriteLocksMu.Unlock()
}

// ensureUploadReaper starts the background reclaim loop the first time an
// upload stream arrives. Starting it lazily keeps agents that never use file
// transfer free of an idle goroutine.
func ensureUploadReaper() {
	uploadReaperOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(uploadReapInterval)
			defer ticker.Stop()
			for now := range ticker.C {
				if removed := reapIdleUploadSessions(now, uploadIdleSessionTTL); removed > 0 {
					log.Printf("[file-transfer] reclaimed %d abandoned upload session(s)", removed)
				}
			}
		}()
	})
}

// uploadStateLastActivity returns the timestamp that decides expiry. Sessions
// recorded before LastActivity was tracked fall back to CreatedAt.
func uploadStateLastActivity(state uploadChunkState) time.Time {
	if !state.LastActivity.IsZero() {
		return state.LastActivity
	}
	return state.CreatedAt
}

// reapIdleUploadSessions removes sessions whose last chunk arrived at least ttl
// ago. It never touches a session with a stream in flight and skips sessions
// whose commit/cancel lock is held, so a slow finalize cannot lose its part
// file. It returns the number of reclaimed sessions.
func reapIdleUploadSessions(now time.Time, ttl time.Duration) int {
	uploadChunksMu.Lock()
	candidates := make([]string, 0, len(uploadChunks))
	for uploadID, state := range uploadChunks {
		last := uploadStateLastActivity(state)
		if last.IsZero() || now.Sub(last) >= ttl {
			candidates = append(candidates, uploadID)
		}
	}
	uploadChunksMu.Unlock()

	removed := 0
	for _, uploadID := range candidates {
		// upload_commit and upload_cancel hold this lock for the whole
		// finalize, so a session being finalized is never reaped underneath it.
		guard := tryAcquireUploadWriteLock(uploadID)
		if guard == nil {
			continue
		}
		reclaimed := reclaimUploadSessionIfIdle(uploadID, now, ttl)
		guard.release()
		if reclaimed {
			removed++
		}
	}
	return removed
}

// reclaimUploadSessionIfIdle deletes the part file and the in-memory entry of
// an idle session.
//
// It holds uploadLifecycleMu across the final check and the deletion.
// beginUploadStream takes the same mutex, so no stream can register between
// "nobody is streaming" and "the part file is gone". Holding only the check and
// releasing before the delete would let a new stream start writing to a file
// that is about to be unlinked.
func reclaimUploadSessionIfIdle(uploadID string, now time.Time, ttl time.Duration) bool {
	uploadLifecycleMu.Lock()
	defer uploadLifecycleMu.Unlock()

	if state := uploadLifecycles[uploadID]; state != nil && state.active > 0 {
		return false
	}
	if uploadReclaimTestHook != nil {
		uploadReclaimTestHook()
	}

	uploadChunksMu.Lock()
	defer uploadChunksMu.Unlock()
	state, exists := uploadChunks[uploadID]
	if !exists {
		return false
	}
	if last := uploadStateLastActivity(state); !last.IsZero() && now.Sub(last) < ttl {
		return false // a chunk landed while this pass was running
	}
	// removeUploadFileLocked keeps the state when the unlink fails, so a part
	// file held open by another process (share violation on Windows) is retried
	// on the next pass instead of leaking silently.
	if err := removeUploadFileLocked(uploadID); err != nil {
		log.Printf("[file-transfer] failed to reclaim upload session upload_id=%s: %v", uploadID, err)
		return false
	}
	return true
}

// liveUploadTempPaths snapshots the part files still owned by an in-memory
// session. Callers must hold uploadLifecycleMu so no new session can appear
// between the snapshot and the deletion that follows it.
func liveUploadTempPaths() []string {
	uploadChunksMu.Lock()
	defer uploadChunksMu.Unlock()
	paths := make([]string, 0, len(uploadChunks))
	for _, state := range uploadChunks {
		if state.TempPath != "" {
			paths = append(paths, state.TempPath)
		}
	}
	return paths
}

// removeStaleUploadParts deletes part files left behind by a previous process.
// A restart drops the in-memory sessions, so their part files would otherwise
// occupy disk forever: upload_cancel can only remove one when the caller still
// knows the target path. Only the files belonging to this target name are
// considered, only when they are older than the idle TTL, and never when a live
// session still owns them.
//
// uploadLifecycleMu is held for the whole scan. Part files are created only
// after beginUploadStream succeeded, so holding the same mutex guarantees the
// live set cannot change while candidates are being removed.
func removeStaleUploadParts(targetPath string, keepPath string, now time.Time) {
	dir := filepath.Dir(filepath.ToSlash(targetPath))
	base := filepath.Base(filepath.ToSlash(targetPath))
	if base == "" || base == "." || base == "/" {
		return
	}
	prefix := "." + base + ".komari-upload-"
	const suffix = ".part"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	uploadLifecycleMu.Lock()
	defer uploadLifecycleMu.Unlock()
	live := liveUploadTempPaths()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		path := filepath.Join(dir, name)
		if keepPath != "" && samePath(path, keepPath) {
			continue
		}
		if isLiveUploadPart(path, live) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || now.Sub(info.ModTime()) < uploadIdleSessionTTL {
			continue
		}
		if removeErr := os.Remove(path); removeErr == nil {
			log.Printf("[file-transfer] removed orphaned upload part %q", path)
		}
	}
}

func isLiveUploadPart(path string, live []string) bool {
	for _, candidate := range live {
		if samePath(candidate, path) {
			return true
		}
	}
	return false
}

// checkUploadSessionCapacity rejects a new session once the agent already holds
// the maximum number of part files. Must be called with uploadChunksMu held.
func checkUploadSessionCapacity() error {
	if len(uploadChunks) >= maxUploadSessions {
		return fmt.Errorf("too many concurrent upload sessions (limit %d)", maxUploadSessions)
	}
	return nil
}
