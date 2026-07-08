package parser

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSQLiteContainerStateIgnoresSubSecondMtimeChanges pins the state's
// timestamp contract: mtime participates at whole-second granularity only,
// so sub-second timestamp instability (filesystems and stat round-trips
// disagree below one second) can never make an unchanged container look
// changed, while a change that crosses a second boundary still registers.
// Content changes are detected by SQLite's own write markers regardless of
// any timestamp.
func TestSQLiteContainerStateIgnoresSubSecondMtimeChanges(t *testing.T) {
	dbPath, _, db := newTestDB(t)
	require.NoError(t, db.Close())

	base := time.Unix(1700000000, 100*int64(time.Millisecond))
	require.NoError(t, os.Chtimes(dbPath, base, base))
	before, ok := StatSQLiteContainerState(dbPath)
	require.True(t, ok, "state must be readable")

	nudged := time.Unix(1700000000, 900*int64(time.Millisecond))
	require.NoError(t, os.Chtimes(dbPath, nudged, nudged))
	within, ok := StatSQLiteContainerState(dbPath)
	require.True(t, ok, "state must be readable after sub-second nudge")
	assert.Equal(t, before, within,
		"a sub-second mtime change must not change the container state")

	crossed := time.Unix(1700000001, 0)
	require.NoError(t, os.Chtimes(dbPath, crossed, crossed))
	afterSecond, ok := StatSQLiteContainerState(dbPath)
	require.True(t, ok, "state must be readable after second boundary")
	assert.NotEqual(t, before, afterSecond,
		"an mtime change across a second boundary must change the state")
}

// TestSQLiteContainerStateRejectsNonSQLiteFiles pins that a file without a
// SQLite header can never be captured, and therefore never trusted as an
// unchanged container.
func TestSQLiteContainerStateRejectsNonSQLiteFiles(t *testing.T) {
	path := t.TempDir() + "/opencode.db"
	require.NoError(t, os.WriteFile(
		path, []byte("not a sqlite database, padded to header size........................................................."), 0o644,
	))
	_, ok := StatSQLiteContainerState(path)
	assert.False(t, ok, "non-SQLite bytes must not produce a container state")
}

// TestOpenCodeProjectsCacheReusesUntilContainerChanges pins the
// project-table cache used by SQLite session parsing: while the container
// state is unchanged the previous load is served back, and any committed
// write invalidates it. Without the cache every parsed session re-queried
// the full project table, which dominated re-parse CPU on large archives.
func TestOpenCodeProjectsCacheReusesUntilContainerChanges(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()
	seeder.AddProject("prj_1", "/home/user/code/app-one")

	first, err := loadOpenCodeProjectsCached(db, dbPath)
	require.NoError(t, err)
	assert.Equal(t,
		map[string]string{"prj_1": "/home/user/code/app-one"}, first)

	// Poison the cached copy to make hits observable: an unchanged
	// container must serve the poisoned entry back, and a changed one must
	// reload the real values from the DB.
	openCodeProjectsCacheMu.Lock()
	entry := openCodeProjectsCache[dbPath]
	entry.projects = map[string]string{"prj_1": "cached-marker"}
	openCodeProjectsCache[dbPath] = entry
	openCodeProjectsCacheMu.Unlock()

	second, err := loadOpenCodeProjectsCached(db, dbPath)
	require.NoError(t, err)
	assert.Equal(t, "cached-marker", second["prj_1"],
		"an unchanged container must be served from the cache")

	_, err = db.Exec(
		"UPDATE project SET worktree = ? WHERE id = ?",
		"/home/user/code/renamed", "prj_1",
	)
	require.NoError(t, err)

	third, err := loadOpenCodeProjectsCached(db, dbPath)
	require.NoError(t, err)
	assert.Equal(t, "/home/user/code/renamed", third["prj_1"],
		"a committed write must invalidate the cached projects")
}
