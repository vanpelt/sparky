//go:build unix

package ext4

import (
	"io/fs"
	"syscall"
)

// allocatedBlocks reports how many 4 KiB ext4 blocks a file's data will need,
// derived from the blocks the host filesystem actually allocated rather than
// from its apparent size. A sparse file — and a container image unpacked from a
// tar can contain them — is charged what it occupies, not what it claims.
//
// st_blocks is in 512-byte units on every Unix, so eight of them make one
// 4 KiB block.
func allocatedBlocks(info fs.FileInfo) int64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return apparentBlocks(info)
	}
	blocks := int64(st.Blocks) / 8
	if blocks == 0 && info.Size() > 0 {
		// Wholly inline or sub-block: still costs one block once written out.
		return 1
	}
	return blocks
}

// hardlinkKey identifies a file by (device, inode) so TreeSize can charge a set
// of hardlinks once. ok is false when the platform did not give us a stat we
// understand, in which case the caller counts every link separately — an
// overestimate, which is the safe direction for a size floor.
func hardlinkKey(info fs.FileInfo) (key [2]uint64, nlink uint64, ok bool) {
	st, sysOK := info.Sys().(*syscall.Stat_t)
	if !sysOK {
		return key, 0, false
	}
	return [2]uint64{uint64(st.Dev), uint64(st.Ino)}, uint64(st.Nlink), true
}
