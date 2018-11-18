// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/petermattis/pebble/db"
)

// fileMetadata holds the metadata for an on-disk table.
type fileMetadata struct {
	// fileNum is the file number.
	fileNum uint64
	// size is the size of the file, in bytes.
	size uint64
	// smallest and largest are the inclusive bounds for the internal keys
	// stored in the table.
	smallest db.InternalKey
	largest  db.InternalKey
	// smallest and largest sequence numbers in the table.
	smallestSeqNum uint64
	largestSeqNum  uint64
	// true if client asked us nicely to compact this file.
	markedForCompaction bool
}

func (m *fileMetadata) tableInfo(dirname string) db.TableInfo {
	return db.TableInfo{
		Path:           dbFilename(dirname, fileTypeTable, m.fileNum),
		FileNum:        m.fileNum,
		Size:           m.size,
		Smallest:       m.smallest,
		Largest:        m.largest,
		SmallestSeqNum: m.smallestSeqNum,
		LargestSeqNum:  m.largestSeqNum,
	}
}

// totalSize returns the total size of all the files in f.
func totalSize(f []fileMetadata) (size uint64) {
	for _, x := range f {
		size += x.size
	}
	return size
}

// ikeyRange returns the minimum smallest and maximum largest internalKey for
// all the fileMetadata in f0 and f1.
func ikeyRange(ucmp db.Compare, f0, f1 []fileMetadata) (smallest, largest db.InternalKey) {
	first := true
	for _, f := range [2][]fileMetadata{f0, f1} {
		for _, meta := range f {
			if first {
				first = false
				smallest, largest = meta.smallest, meta.largest
				continue
			}
			if db.InternalCompare(ucmp, meta.smallest, smallest) < 0 {
				smallest = meta.smallest
			}
			if db.InternalCompare(ucmp, meta.largest, largest) > 0 {
				largest = meta.largest
			}
		}
	}
	return smallest, largest
}

type byFileNum []fileMetadata

func (b byFileNum) Len() int           { return len(b) }
func (b byFileNum) Less(i, j int) bool { return b[i].fileNum < b[j].fileNum }
func (b byFileNum) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }

type bySmallest struct {
	dat []fileMetadata
	cmp db.Compare
}

func (b bySmallest) Len() int { return len(b.dat) }
func (b bySmallest) Less(i, j int) bool {
	return db.InternalCompare(b.cmp, b.dat[i].smallest, b.dat[j].smallest) < 0
}
func (b bySmallest) Swap(i, j int) { b.dat[i], b.dat[j] = b.dat[j], b.dat[i] }

const numLevels = 7

// version is a collection of file metadata for on-disk tables at various
// levels. In-memory DBs are written to level-0 tables, and compactions
// migrate data from level N to level N+1. The tables map internal keys (which
// are a user key, a delete or set bit, and a sequence number) to user values.
//
// The tables at level 0 are sorted by increasing fileNum. If two level 0
// tables have fileNums i and j and i < j, then the sequence numbers of every
// internal key in table i are all less than those for table j. The range of
// internal keys [fileMetadata.smallest, fileMetadata.largest] in each level 0
// table may overlap.
//
// The tables at any non-0 level are sorted by their internal key range and any
// two tables at the same non-0 level do not overlap.
//
// The internal key ranges of two tables at different levels X and Y may
// overlap, for any X != Y.
//
// Finally, for every internal key in a table at level X, there is no internal
// key in a higher level table that has both the same user key and a higher
// sequence number.
type version struct {
	refs int32

	files [numLevels][]fileMetadata

	// The list the version is linked into.
	list *versionList

	// The next/prev link for the versionList doubly-linked list of versions.
	prev, next *version
}

func (v *version) String() string {
	var buf bytes.Buffer
	for level := 0; level < numLevels; level++ {
		if len(v.files[level]) == 0 {
			continue
		}
		fmt.Fprintf(&buf, "%d:", level)
		for j := range v.files[level] {
			f := &v.files[level][j]
			fmt.Fprintf(&buf, " %s-%s", f.smallest.UserKey, f.largest.UserKey)
		}
		fmt.Fprintf(&buf, "\n")
	}
	return buf.String()
}

func (v *version) ref() {
	atomic.AddInt32(&v.refs, 1)
}

func (v *version) unref() {
	if atomic.AddInt32(&v.refs, -1) == 0 {
		l := v.list
		l.mu.Lock()
		l.remove(v)
		l.mu.Unlock()
	}
}

func (v *version) unrefLocked() {
	if atomic.AddInt32(&v.refs, -1) == 0 {
		v.list.remove(v)
	}
}

// overlaps returns all elements of v.files[level] whose user key range
// intersects the inclusive range [start, end]. If level is non-zero then the
// user key ranges of v.files[level] are assumed to not overlap (although they
// may touch). If level is zero then that assumption cannot be made, and the
// [start, end] range is expanded to the union of those matching ranges so far
// and the computation is repeated until [start, end] stabilizes.
func (v *version) overlaps(
	level int, cmp db.Compare, start, end []byte,
) (ret []fileMetadata) {
	if level == 0 {
		// The sstables in level 0 can overlap with each other. As soon as we find
		// one sstable that overlaps with our target range, we need to expand the
		// range and find all sstables that overlap with the expanded range.
	loop:
		for {
			for _, meta := range v.files[level] {
				smallest := meta.smallest.UserKey
				largest := meta.largest.UserKey
				if cmp(largest, start) < 0 {
					// meta is completely before the specified range; skip it.
					continue
				}
				if cmp(smallest, end) > 0 {
					// meta is completely after the specified range; skip it.
					continue
				}
				ret = append(ret, meta)

				// If level == 0, check if the newly added fileMetadata has
				// expanded the range. If so, restart the search.
				restart := false
				if cmp(smallest, start) < 0 {
					start = smallest
					restart = true
				}
				if cmp(largest, end) > 0 {
					end = largest
					restart = true
				}
				if restart {
					ret = ret[:0]
					continue loop
				}
			}
			return ret
		}
	}

	// Binary search to find the range of files which overlaps with our target
	// range.
	files := v.files[level]
	lower := sort.Search(len(files), func(i int) bool {
		return cmp(files[i].largest.UserKey, start) >= 0
	})
	upper := sort.Search(len(files), func(i int) bool {
		return cmp(files[i].smallest.UserKey, end) > 0
	})
	return files[lower:upper]
}

// checkOrdering checks that the files are consistent with respect to
// increasing file numbers (for level 0 files) and increasing and non-
// overlapping internal key ranges (for level non-0 files).
func (v *version) checkOrdering(cmp db.Compare) error {
	for level, ff := range v.files {
		if level == 0 {
			prevFileNum := uint64(0)
			for i, f := range ff {
				if i != 0 && prevFileNum >= f.fileNum {
					return fmt.Errorf("level 0 files are not in increasing fileNum order: %d, %d", prevFileNum, f.fileNum)
				}
				prevFileNum = f.fileNum
			}
		} else {
			var prevLargest db.InternalKey
			for i, f := range ff {
				if i != 0 && db.InternalCompare(cmp, prevLargest, f.smallest) >= 0 {
					return fmt.Errorf("level non-0 files are not in increasing ikey order: %q, %q", prevLargest, f.smallest)
				}
				if db.InternalCompare(cmp, f.smallest, f.largest) > 0 {
					return fmt.Errorf("level non-0 file has inconsistent bounds: %q, %q", f.smallest, f.largest)
				}
				prevLargest = f.largest
			}
		}
	}
	return nil
}

// tableNewIter creates a new iterator for the given file number.
type tableNewIter func(meta *fileMetadata) (db.InternalIterator, error)

type versionList struct {
	mu   *sync.Mutex
	root version
}

func (l *versionList) init() {
	l.root.next = &l.root
	l.root.prev = &l.root
}

func (l *versionList) empty() bool {
	return l.root.next == &l.root
}

func (l *versionList) front() *version {
	return l.root.next
}

func (l *versionList) back() *version {
	return l.root.prev
}

func (l *versionList) pushBack(v *version) {
	if v.list != nil || v.prev != nil || v.next != nil {
		panic("pebble: version list is inconsistent")
	}
	v.prev = l.root.prev
	v.prev.next = v
	v.next = &l.root
	v.next.prev = v
	v.list = l
}

func (l *versionList) remove(v *version) {
	if v == &l.root {
		panic("pebble: cannot remove version list root node")
	}
	if v.list != l {
		panic("pebble: version list is inconsistent")
	}
	v.prev.next = v.next
	v.next.prev = v.prev
	v.next = nil // avoid memory leaks
	v.prev = nil // avoid memory leaks
	v.list = nil // avoid memory leaks
}
