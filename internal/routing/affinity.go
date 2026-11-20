// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"crypto/sha256"
	"encoding/binary"
)

// preferredTarget returns the target a session prefers for one route, using
// rendezvous hashing (highest random weight) over the target *names*.
//
// H(route || session || targetName) is stable across process restarts and
// reloads, independent of slice order, and adding or removing one target only
// remaps the sessions that preferred that target. A plain modulo over the
// target count would remap nearly every session on a size change.
func preferredTarget(route, session string, targets []*Target) *Target {
	if session == "" || len(targets) == 0 {
		return nil
	}
	prefix := make([]byte, 0, len(route)+len(session)+len(targets[0].Name)+2)
	prefix = append(prefix, route...)
	prefix = append(prefix, 0)
	prefix = append(prefix, session...)
	prefix = append(prefix, 0)

	var best *Target
	var bestScore uint64
	for _, t := range targets {
		sum := sha256.Sum256(append(append([]byte(nil), prefix...), t.Name...))
		score := binary.BigEndian.Uint64(sum[:8])
		if best == nil || score > bestScore || (score == bestScore && t.Name < best.Name) {
			best, bestScore = t, score
		}
	}
	return best
}
