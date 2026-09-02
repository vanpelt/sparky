//go:build !unix

package ext4

import "io/fs"

// allocatedBlocks falls back to the apparent size where no Unix stat is
// available. Sparkbox builds and runs its host side on Unix; this exists so the
// package still compiles for cross-platform tooling.
func allocatedBlocks(info fs.FileInfo) int64 { return apparentBlocks(info) }

// hardlinkKey cannot identify hardlinks without a Unix stat, so every link is
// counted separately — an overestimate, which is the safe direction for a floor.
func hardlinkKey(info fs.FileInfo) (key [2]uint64, nlink uint64, ok bool) {
	return key, 0, false
}
