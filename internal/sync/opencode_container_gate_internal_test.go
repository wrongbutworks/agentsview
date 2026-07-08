package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

// TestSQLiteContainerPassPromotesOnlyPreDiscoveryCaptures pins the gate's
// ordering invariant: the state promoted to trusted must have been captured
// BEFORE discovery listed the container's sessions. Discovery reads the
// session rows first, so a state captured afterwards can be newer than the
// discovered set — a session written in between would then be gate-skipped
// forever without ever being parsed. Containers with no pre-discovery
// capture must therefore never be promoted, and promoted states must be
// exactly the pre-discovery ones.
func TestSQLiteContainerPassPromotesOnlyPreDiscoveryCaptures(t *testing.T) {
	files := []parser.DiscoveredFile{
		{Agent: parser.AgentOpenCode, Path: "/data/opencode.db#ses-1"},
		{Agent: parser.AgentOpenCode, Path: "/data/opencode.db#ses-2"},
	}

	t.Run("missing pre-discovery capture blocks promotion", func(t *testing.T) {
		e := &Engine{}
		e.beginSQLiteContainerPass(
			files, map[string]parser.SQLiteContainerState{},
		)
		e.noteSQLiteContainerResult("/data/opencode.db#ses-1", true)
		e.noteSQLiteContainerResult("/data/opencode.db#ses-2", true)
		e.finishSQLiteContainerPass(false)
		assert.Empty(t, e.trustedSQLiteContainers,
			"a container without a pre-discovery capture must not be trusted")
	})

	t.Run("promoted state is the pre-discovery capture", func(t *testing.T) {
		e := &Engine{}
		pre := parser.SQLiteContainerState{
			DBSize: 4096, DBMtimeSec: 1700000000, DBChangeCounter: 41,
		}
		e.beginSQLiteContainerPass(
			files,
			map[string]parser.SQLiteContainerState{"/data/opencode.db": pre},
		)
		e.noteSQLiteContainerResult("/data/opencode.db#ses-1", true)
		e.noteSQLiteContainerResult("/data/opencode.db#ses-2", true)
		e.finishSQLiteContainerPass(false)
		require.Contains(t, e.trustedSQLiteContainers, "/data/opencode.db")
		assert.Equal(t, pre, e.trustedSQLiteContainers["/data/opencode.db"],
			"trusted state must be exactly the pre-discovery capture")
	})
}
