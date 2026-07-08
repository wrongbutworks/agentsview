// ABOUTME: Change-detection state for shared SQLite container files, built
// ABOUTME: on SQLite's own write markers rather than timestamp precision.
package parser

import (
	"encoding/binary"
	"os"
)

// SQLiteContainerState captures a shared SQLite container file's
// change-detection state. Two equal states mean the container provably has
// not changed between the two captures.
//
// "Provably" deliberately does not rest on timestamp equality. Filesystem
// mtime granularity varies (ns on APFS/ext4, 1s on HFS+, 2s on FAT) and
// timestamps round-trip through layers with different precisions, so
// comparisons finer than one second are not meaningful; mtimes here are
// truncated to whole seconds and act only as coarse extra signal. The real
// precision comes from SQLite's own write markers, which advance on every
// committed transaction regardless of any clock:
//
//   - the 32-bit file change counter at byte 24 of the database header
//     (bumped per transaction in rollback-journal mode, and on every
//     checkpoint in WAL mode), and
//   - the WAL header's checkpoint sequence number and random salts, plus
//     the WAL size: between WAL resets commits only append frames (the WAL
//     grows), and every WAL reset re-randomizes the salts.
//
// A spurious mismatch merely costs one redundant re-read, while a wrong
// match is what the write markers rule out.
type SQLiteContainerState struct {
	DBSize          int64
	DBMtimeSec      int64
	DBChangeCounter uint32
	WALSize         int64
	WALMtimeSec     int64
	WALCkptSeq      uint32
	WALSalt1        uint32
	WALSalt2        uint32
}

// sqliteHeaderProbeSize covers the 100-byte SQLite database header; the
// file change counter lives at bytes 24-27 (big-endian).
const sqliteHeaderProbeSize = 100

var sqliteHeaderMagic = []byte("SQLite format 3\x00")

// StatSQLiteContainerState captures the current change-detection state of a
// shared SQLite container. ok is false when the container is missing or its
// headers cannot be read, in which case the container must never be treated
// as unchanged.
func StatSQLiteContainerState(dbPath string) (SQLiteContainerState, bool) {
	info, err := os.Stat(dbPath)
	if err != nil || !info.Mode().IsRegular() {
		return SQLiteContainerState{}, false
	}
	state := SQLiteContainerState{
		DBSize:     info.Size(),
		DBMtimeSec: info.ModTime().Unix(),
	}
	counter, ok := readSQLiteChangeCounter(dbPath)
	if !ok {
		return SQLiteContainerState{}, false
	}
	state.DBChangeCounter = counter

	walPath := dbPath + "-wal"
	walInfo, err := os.Stat(walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return state, true
		}
		return SQLiteContainerState{}, false
	}
	if !walInfo.Mode().IsRegular() ||
		walInfo.Size() <= sqliteWALHeaderSize {
		// A WAL at or under header size carries no transaction frames, so
		// it is equivalent to an absent WAL: read-only SQLite clients can
		// leave an empty WAL behind without implying any content change.
		return state, true
	}
	header := make([]byte, sqliteWALHeaderSize)
	f, err := os.Open(walPath)
	if err != nil {
		return SQLiteContainerState{}, false
	}
	defer f.Close()
	if _, err := f.ReadAt(header, 0); err != nil {
		return SQLiteContainerState{}, false
	}
	state.WALSize = walInfo.Size()
	state.WALMtimeSec = walInfo.ModTime().Unix()
	state.WALCkptSeq = binary.BigEndian.Uint32(header[12:16])
	state.WALSalt1 = binary.BigEndian.Uint32(header[16:20])
	state.WALSalt2 = binary.BigEndian.Uint32(header[20:24])
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
