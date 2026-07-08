// ABOUTME: Container-level freshness gate for OpenCode-family shared
// ABOUTME: SQLite databases, skipping per-session re-parse on idle syncs.
package sync

import (
	"encoding/binary"
	"os"

	"go.kenn.io/agentsview/internal/parser"
)

// The OpenCode-family providers fan one shared SQLite database into one
// virtual source per session row. Per-session freshness cannot be decided
// before parsing (a message or part row can change without bumping the
// session's time_updated, which is why dropUnchangedSharedSQLiteResults
// compares content fingerprints after the parse), so a periodic sync of an
// untouched archive used to re-open and re-parse every session on every
// pass. The gate restores an O(1) answer for the common idle case: when the
// container file provably has not changed since a pass that verified every
// one of its sessions, none of its sessions can have changed either, and
// they all skip before fingerprinting.
//
// "Provably" deliberately does not rest on timestamp equality. Filesystem
// mtime granularity varies (ns on APFS/ext4, 1s on HFS+, 2s on FAT), so two
// different container states can carry equal mtimes. The state instead
// folds in SQLite's own write markers, which advance on every committed
// transaction regardless of any clock:
//
//   - the 32-bit file change counter at byte 24 of the database header
//     (bumped per transaction in rollback-journal mode, and on every
//     checkpoint in WAL mode), and
//   - the WAL header's checkpoint sequence number and random salts, plus
//     the WAL size: between WAL resets commits only append frames (the WAL
//     grows), and every WAL reset re-randomizes the salts.
//
// File sizes and mtimes still participate as extra signal: a spurious
// mismatch merely costs one re-parse, while a wrong match is what the
// write markers rule out.
type sqliteContainerState struct {
	dbSize          int64
	dbMtimeNS       int64
	dbChangeCounter uint32
	walSize         int64
	walMtimeNS      int64
	walCkptSeq      uint32
	walSalt1        uint32
	walSalt2        uint32
}

const (
	// sqliteHeaderProbeSize covers the 100-byte SQLite database header;
	// the file change counter lives at bytes 24-27 (big-endian).
	sqliteHeaderProbeSize = 100
	// sqliteGateWALHeaderSize is the 32-byte WAL header. A WAL at or under
	// this size carries no transaction frames, so it is equivalent to an
	// absent WAL: read-only SQLite clients can leave an empty WAL behind
	// without implying any content change.
	sqliteGateWALHeaderSize = 32
)

var sqliteHeaderMagic = []byte("SQLite format 3\x00")

// statSQLiteContainerState captures the current change-detection state of a
// shared SQLite container. ok is false when the container is missing or its
// headers cannot be read, in which case the container must never be
// trusted as unchanged.
func statSQLiteContainerState(dbPath string) (sqliteContainerState, bool) {
	info, err := os.Stat(dbPath)
	if err != nil || !info.Mode().IsRegular() {
		return sqliteContainerState{}, false
	}
	state := sqliteContainerState{
		dbSize:    info.Size(),
		dbMtimeNS: info.ModTime().UnixNano(),
	}
	counter, ok := readSQLiteChangeCounter(dbPath)
	if !ok {
		return sqliteContainerState{}, false
	}
	state.dbChangeCounter = counter

	walPath := dbPath + "-wal"
	walInfo, err := os.Stat(walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return state, true
		}
		return sqliteContainerState{}, false
	}
	if !walInfo.Mode().IsRegular() ||
		walInfo.Size() <= sqliteGateWALHeaderSize {
		// Frameless WALs are indistinguishable from absent ones; see
		// sqliteGateWALHeaderSize.
		return state, true
	}
	header := make([]byte, sqliteGateWALHeaderSize)
	f, err := os.Open(walPath)
	if err != nil {
		return sqliteContainerState{}, false
	}
	defer f.Close()
	if _, err := f.ReadAt(header, 0); err != nil {
		return sqliteContainerState{}, false
	}
	state.walSize = walInfo.Size()
	state.walMtimeNS = walInfo.ModTime().UnixNano()
	state.walCkptSeq = binary.BigEndian.Uint32(header[12:16])
	state.walSalt1 = binary.BigEndian.Uint32(header[16:20])
	state.walSalt2 = binary.BigEndian.Uint32(header[20:24])
	return state, true
}

func readSQLiteChangeCounter(dbPath string) (uint32, bool) {
	f, err := os.Open(dbPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	header := make([]byte, sqliteHeaderProbeSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		return 0, false
	}
	for i, b := range sqliteHeaderMagic {
		if header[i] != b {
			return 0, false
		}
	}
	return binary.BigEndian.Uint32(header[24:28]), true
}

// openCodeFamilySQLiteAgents lists the agents whose sessions live in a
// shared OpenCode-format SQLite container.
var openCodeFamilySQLiteAgents = []parser.AgentType{
	parser.AgentOpenCode,
	parser.AgentKilo,
	parser.AgentMiMoCode,
	parser.AgentIcodemate,
}

// sqliteContainerPathForFile maps a discovered file to its shared SQLite
// container path, or "" when the file is not an OpenCode-family SQLite
// virtual source (storage-mode JSON sessions included).
func sqliteContainerPathForFile(file parser.DiscoveredFile) string {
	dbName := openCodeFormatDBName(file.Agent)
	if dbName == "" {
		return ""
	}
	dbPath, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, dbName)
	if !ok {
		return ""
	}
	return dbPath
}

// sqliteContainerPathForResultPath maps a processed result path back to its
// container. Result paths arrive without an agent, so every family DB name
// is tried.
func sqliteContainerPathForResultPath(path string) string {
	for _, agent := range openCodeFamilySQLiteAgents {
		dbPath, _, ok := parser.ParseVirtualSourcePathForBase(
			path, openCodeFormatDBName(agent),
		)
		if ok {
			return dbPath
		}
	}
	return ""
}

// sqliteContainerPass tracks one sync pass's view of every OpenCode-family
// SQLite container it discovered. captured is written once before workers
// start and is read-only afterwards; completed and failed are touched only
// by the single collectAndBatch goroutine, so no locking is needed during
// the pass.
type sqliteContainerPass struct {
	captured   map[string]sqliteContainerState
	discovered map[string]int
	completed  map[string]int
	failed     map[string]bool
	poisoned   bool
}

// beginSQLiteContainerPass captures the container states for the discovered
// files of a starting sync pass. It must be called before the pass's
// workers start so that any write racing the pass lands after the capture
// and therefore invalidates it on the next comparison. files must be the
// pre-filter discovery set: promotion requires seeing a completion for
// every discovered session, so an mtime-cutoff or scope filter that drops
// sessions from processing keeps the container untrusted.
func (e *Engine) beginSQLiteContainerPass(files []parser.DiscoveredFile) {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	e.containerPass = nil
	if e.forceParse {
		return
	}
	var pass *sqliteContainerPass
	for _, file := range files {
		dbPath := sqliteContainerPathForFile(file)
		if dbPath == "" {
			continue
		}
		if pass == nil {
			pass = &sqliteContainerPass{
				captured:   make(map[string]sqliteContainerState),
				discovered: make(map[string]int),
				completed:  make(map[string]int),
				failed:     make(map[string]bool),
			}
		}
		pass.discovered[dbPath]++
		if _, seen := pass.captured[dbPath]; !seen {
			if state, ok := statSQLiteContainerState(dbPath); ok {
				pass.captured[dbPath] = state
			} else {
				pass.failed[dbPath] = true
			}
		}
	}
	e.containerPass = pass
}

// sqliteContainerSourceFresh reports whether a discovered file belongs to a
// container whose current state matches the last fully verified state, in
// which case the session is unchanged and skips before fingerprinting.
func (e *Engine) sqliteContainerSourceFresh(file parser.DiscoveredFile) bool {
	if e.forceParse || file.ForceParse {
		return false
	}
	dbPath := sqliteContainerPathForFile(file)
	if dbPath == "" {
		return false
	}
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	if e.containerPass == nil {
		return false
	}
	current, ok := e.containerPass.captured[dbPath]
	if !ok {
		return false
	}
	trusted, ok := e.trustedSQLiteContainers[dbPath]
	return ok && current == trusted
}

// noteSQLiteContainerResult records a processed file's outcome for
// promotion bookkeeping. Skips count as completions: a skipped session was
// either gate-skipped against an already-trusted state or individually
// verified fresh.
func (e *Engine) noteSQLiteContainerResult(path string, ok bool) {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	pass := e.containerPass
	if pass == nil {
		return
	}
	dbPath := sqliteContainerPathForResultPath(path)
	if dbPath == "" {
		return
	}
	if ok {
		pass.completed[dbPath]++
	} else {
		pass.failed[dbPath] = true
	}
}

// poisonSQLiteContainerPass blocks every promotion for the current pass.
// Used when a batched DB write fails, because batch failures cannot be
// attributed to individual sessions.
func (e *Engine) poisonSQLiteContainerPass() {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	if e.containerPass != nil {
		e.containerPass.poisoned = true
	}
}

// finishSQLiteContainerPass promotes the pass's captured container states
// to trusted for every container whose discovered sessions all completed
// without errors, retries, or write failures. incomplete marks passes that
// must never promote (aborted, cancelled, or discovery failures whose
// provider cannot be attributed).
func (e *Engine) finishSQLiteContainerPass(incomplete bool) {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	pass := e.containerPass
	e.containerPass = nil
	if pass == nil || pass.poisoned || incomplete {
		return
	}
	for dbPath, state := range pass.captured {
		if pass.failed[dbPath] {
			continue
		}
		if pass.completed[dbPath] != pass.discovered[dbPath] {
			continue
		}
		if e.trustedSQLiteContainers == nil {
			e.trustedSQLiteContainers =
				make(map[string]sqliteContainerState)
		}
		e.trustedSQLiteContainers[dbPath] = state
	}
}

// clearTrustedSQLiteContainers drops every trusted container state. Called
// by resync, which rebuilds the archive from scratch and must re-verify
// every session against it.
func (e *Engine) clearTrustedSQLiteContainers() {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	e.trustedSQLiteContainers = nil
	e.containerPass = nil
}
