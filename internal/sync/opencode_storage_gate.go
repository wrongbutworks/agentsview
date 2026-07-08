// ABOUTME: Per-session freshness gate for OpenCode-family file-backed
// ABOUTME: storage sessions, skipping re-parse when the tree is unchanged.
package sync

import (
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// File-backed OpenCode-format sessions fan one session JSON plus its
// message and part files into a single source whose parse re-reads the
// whole tree. They are deliberately excluded from the mtime-keyed skip
// cache (see shouldCacheSkip): the cache keys on a composite max-mtime,
// which cannot see a child rewritten within one filesystem mtime granule
// or with a restored timestamp. The consequence was that every full sync
// and unwatched-root poll re-read and re-parsed every storage session of
// an idle archive, and every watcher event on a streaming session
// re-parsed its entire history.
//
// The gate restores a cheap answer for the unchanged case using
// parser.StatOpenCodeStorageSessionState, a per-file (name, size, mtimeNS)
// signature over exactly the files the parse would read. A session is
// skipped before fingerprinting only when its current signature equals one
// captured before a parse that the engine then verified against the stored
// archive (every parsed result dropped as unchanged). The remaining blind
// spot — an in-place child rewrite preserving both size and mtime — is
// covered on the watcher path by invalidating trust for a session whenever
// a changed-path event resolves to it, so an event-signaled change always
// re-verifies by content. Trust is in-memory only: a restart re-verifies
// every session once, mirroring the shared-SQLite container gate.

// openCodeStorageSessionPath returns the discovered file's path when it is
// a file-backed OpenCode-format storage session JSON under a storage-mode
// root of its agent, or "" otherwise (SQLite containers and virtual rows
// included). The root-mode walk mirrors shouldCacheSkip, which excludes
// these same paths from the mtime skip cache.
func (e *Engine) openCodeStorageSessionPath(file parser.DiscoveredFile) string {
	if !isOpenCodeFormatStorageAgent(file.Agent) {
		return ""
	}
	if filepath.Base(file.Path) == openCodeFormatDBName(file.Agent) {
		return ""
	}
	if isOpenCodeFormatSQLiteVirtualPath(file.Agent, file.Path) {
		return ""
	}
	for _, dir := range e.agentDirs[file.Agent] {
		if dir == "" {
			continue
		}
		src := resolveOpenCodeFormatSource(file.Agent, dir)
		if src.Mode != parser.OpenCodeSourceStorage {
			continue
		}
		rel, ok := isUnder(dir, file.Path)
		if !ok {
			continue
		}
		rel = filepath.ToSlash(rel)
		sessionPrefix := "storage/" + filepath.Base(src.SessionRoot) + "/"
		if strings.HasPrefix(rel, sessionPrefix) {
			return file.Path
		}
		return ""
	}
	return ""
}

// openCodeStorageSessionGateState captures the session's current stat
// signature for the gate. It returns ("", false) when the file is not a
// gateable storage session, this run force-parses, or the signature cannot
// be captured; such sessions never skip and never promote.
func (e *Engine) openCodeStorageSessionGateState(
	file parser.DiscoveredFile,
) (string, bool) {
	if e.forceParse || file.ForceParse {
		return "", false
	}
	sessionPath := e.openCodeStorageSessionPath(file)
	if sessionPath == "" {
		return "", false
	}
	state, ok := parser.StatOpenCodeStorageSessionState(sessionPath)
	if !ok {
		return "", false
	}
	return state, true
}

// openCodeStorageSessionFresh reports whether the session's captured stat
// signature matches the last verified one, in which case its parse inputs
// are unchanged and it skips before fingerprinting.
func (e *Engine) openCodeStorageSessionFresh(path, state string) bool {
	if state == "" {
		return false
	}
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	trusted, ok := e.trustedStorageSessions[path]
	return ok && trusted == state
}

// promoteOpenCodeStorageSession records a stat signature that was captured
// before a parse whose every result the engine verified as already stored.
// The capture-before-parse ordering makes races safe: a write landing
// between capture and parse leaves the parsed content newer than the
// signature, so the next capture mismatches and re-verifies.
func (e *Engine) promoteOpenCodeStorageSession(path, state string) {
	if path == "" || state == "" {
		return
	}
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	if e.trustedStorageSessions == nil {
		e.trustedStorageSessions = make(map[string]string)
	}
	e.trustedStorageSessions[path] = state
}

// invalidateOpenCodeStorageSession drops trust for one session. Called when
// a pass parses it to a different outcome and, critically, when a watcher
// changed-path event resolves to it: the event says something changed even
// if the stat signature cannot see it, so the next pass must re-verify by
// content.
func (e *Engine) invalidateOpenCodeStorageSession(path string) {
	if path == "" {
		return
	}
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	delete(e.trustedStorageSessions, path)
}

// clearTrustedOpenCodeStorageSessions drops every trusted session state.
// Called by resync, which rebuilds the archive from scratch and must
// re-verify every session against it.
func (e *Engine) clearTrustedOpenCodeStorageSessions() {
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	e.trustedStorageSessions = nil
}
