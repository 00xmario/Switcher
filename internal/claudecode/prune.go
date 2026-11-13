package claudecode

import (
	"os"
	"path/filepath"
	"time"
)

// RecoveryKept is how long the recovery copies of Claude Code logins are
// kept: the snapshot each switch saves, and the generations set aside when a
// newer login is adopted. They hold whole logins, with the tokens of any MCP
// servers signed in to, so they do not pile up for good; a refresh token of
// that age was replaced long ago, and the copy no longer recovers anything.
const RecoveryKept = 30 * 24 * time.Hour

// PruneRecovery removes recovery copies older than keep and reports how many
// it removed. While a switch is unfinished nothing is removed: its copy is
// what recovery restores.
func (m *Manager) PruneRecovery(keep time.Duration) int {
	if pending, err := readRecovery(m.pendingPath()); err != nil || pending.Exists {
		return 0
	}
	cutoff := time.Now().Add(-keep)
	removed := 0
	for _, kind := range []string{"backups", "generations"} {
		dir := filepath.Join(m.dataRoot, kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			// As the entry itself is, never what a link points to.
			info, err := entry.Info()
			if err != nil || info.Mode()&os.ModeSymlink != 0 || info.ModTime().After(cutoff) {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			switch {
			case kind == "backups" && info.IsDir():
				err = os.RemoveAll(path)
			case kind == "generations" && info.Mode().IsRegular():
				err = os.Remove(path)
			default:
				continue
			}
			if err == nil {
				removed++
			}
		}
	}
	return removed
}
