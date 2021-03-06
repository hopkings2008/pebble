// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package manifest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
)

// TODO(peter): describe the MANIFEST file format, independently of the C++
// project.

var errCorruptManifest = errors.New("pebble: corrupt manifest")

type byteReader interface {
	io.ByteReader
	io.Reader
}

// Tags for the versionEdit disk format.
// Tag 8 is no longer used.
const (
	// LevelDB tags.
	tagComparator     = 1
	tagLogNumber      = 2
	tagNextFileNumber = 3
	tagLastSequence   = 4
	tagCompactPointer = 5
	tagDeletedFile    = 6
	tagNewFile        = 7
	tagPrevLogNumber  = 9

	// RocksDB tags.
	tagNewFile2         = 100
	tagNewFile3         = 102
	tagNewFile4         = 103
	tagColumnFamily     = 200
	tagColumnFamilyAdd  = 201
	tagColumnFamilyDrop = 202
	tagMaxColumnFamily  = 203

	// The custom tags sub-format used by tagNewFile4.
	customTagTerminate         = 1
	customTagNeedsCompaction   = 2
	customTagCreationTime      = 6
	customTagPathID            = 65
	customTagNonSafeIgnoreMask = 1 << 6
)

// DeletedFileEntry holds the state for a file deletion from a level. The file
// itself might still be referenced by another level.
type DeletedFileEntry struct {
	Level   int
	FileNum base.FileNum
}

// NewFileEntry holds the state for a new file or one moved from a different
// level.
type NewFileEntry struct {
	Level int
	Meta  *FileMetadata
}

// VersionEdit holds the state for an edit to a Version along with other
// on-disk state (log numbers, next file number, and the last sequence number).
type VersionEdit struct {
	// ComparerName is the value of Options.Comparer.Name. This is only set in
	// the first VersionEdit in a manifest (either when the DB is created, or
	// when a new manifest is created) and is used to verify that the comparer
	// specified at Open matches the comparer that was previously used.
	ComparerName string

	// MinUnflushedLogNum is the smallest WAL log file number corresponding to
	// mutations that have not been flushed to an sstable.
	//
	// This is an optional field, and 0 represents it is not set.
	MinUnflushedLogNum base.FileNum

	// ObsoletePrevLogNum is a historic artifact from LevelDB that is not used by
	// Pebble, RocksDB, or even LevelDB. Its use in LevelDB was deprecated in
	// 6/2011. We keep it around purely for informational purposes when
	// displaying MANIFEST contents.
	ObsoletePrevLogNum uint64

	// The next file number. A single counter is used to assign file numbers
	// for the WAL, MANIFEST, sstable, and OPTIONS files.
	NextFileNum base.FileNum

	// LastSeqNum is an upper bound on the sequence numbers that have been
	// assigned in flushed WALs. Unflushed WALs (that will be replayed during
	// recovery) may contain sequence numbers greater than this value.
	LastSeqNum uint64

	// A file num may be present in both deleted files and new files when it
	// is moved from a lower level to a higher level (when the compaction
	// found that there was no overlapping file at the higher level).
	DeletedFiles map[DeletedFileEntry]bool
	NewFiles     []NewFileEntry
}

// Decode decodes an edit from the specified reader.
func (v *VersionEdit) Decode(r io.Reader) error {
	br, ok := r.(byteReader)
	if !ok {
		br = bufio.NewReader(r)
	}
	d := versionEditDecoder{br}
	for {
		tag, err := binary.ReadUvarint(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch tag {
		case tagComparator:
			s, err := d.readBytes()
			if err != nil {
				return err
			}
			v.ComparerName = string(s)

		case tagLogNumber:
			n, err := d.readFileNum()
			if err != nil {
				return err
			}
			v.MinUnflushedLogNum = n

		case tagNextFileNumber:
			n, err := d.readFileNum()
			if err != nil {
				return err
			}
			v.NextFileNum = n

		case tagLastSequence:
			n, err := d.readUvarint()
			if err != nil {
				return err
			}
			v.LastSeqNum = n

		case tagCompactPointer:
			if _, err := d.readLevel(); err != nil {
				return err
			}
			if _, err := d.readBytes(); err != nil {
				return err
			}
			// NB: RocksDB does not use compaction pointers anymore.

		case tagDeletedFile:
			level, err := d.readLevel()
			if err != nil {
				return err
			}
			fileNum, err := d.readFileNum()
			if err != nil {
				return err
			}
			if v.DeletedFiles == nil {
				v.DeletedFiles = make(map[DeletedFileEntry]bool)
			}
			v.DeletedFiles[DeletedFileEntry{level, fileNum}] = true

		case tagNewFile, tagNewFile2, tagNewFile3, tagNewFile4:
			level, err := d.readLevel()
			if err != nil {
				return err
			}
			fileNum, err := d.readFileNum()
			if err != nil {
				return err
			}
			if tag == tagNewFile3 {
				// The pathID field appears unused in RocksDB.
				_ /* pathID */, err := d.readUvarint()
				if err != nil {
					return err
				}
			}
			size, err := d.readUvarint()
			if err != nil {
				return err
			}
			smallest, err := d.readBytes()
			if err != nil {
				return err
			}
			largest, err := d.readBytes()
			if err != nil {
				return err
			}
			var smallestSeqNum uint64
			var largestSeqNum uint64
			if tag != tagNewFile {
				smallestSeqNum, err = d.readUvarint()
				if err != nil {
					return err
				}
				largestSeqNum, err = d.readUvarint()
				if err != nil {
					return err
				}
			}
			var markedForCompaction bool
			var creationTime uint64
			if tag == tagNewFile4 {
				for {
					customTag, err := d.readUvarint()
					if err != nil {
						return err
					}
					if customTag == customTagTerminate {
						break
					}
					field, err := d.readBytes()
					if err != nil {
						return err
					}
					switch customTag {
					case customTagNeedsCompaction:
						if len(field) != 1 {
							return errors.New("new-file4: need-compaction field wrong size")
						}
						markedForCompaction = (field[0] == 1)

					case customTagCreationTime:
						var n int
						creationTime, n = binary.Uvarint(field)
						if n != len(field) {
							return errors.New("new-file4: invalid file creation time")
						}

					case customTagPathID:
						return errors.New("new-file4: path-id field not supported")

					default:
						if (customTag & customTagNonSafeIgnoreMask) != 0 {
							return errors.Errorf("new-file4: custom field not supported: %d", customTag)
						}
					}
				}
			}
			v.NewFiles = append(v.NewFiles, NewFileEntry{
				Level: level,
				Meta: &FileMetadata{
					FileNum:             fileNum,
					Size:                size,
					CreationTime:        int64(creationTime),
					Smallest:            base.DecodeInternalKey(smallest),
					Largest:             base.DecodeInternalKey(largest),
					SmallestSeqNum:      smallestSeqNum,
					LargestSeqNum:       largestSeqNum,
					MarkedForCompaction: markedForCompaction,
				},
			})

		case tagPrevLogNumber:
			n, err := d.readUvarint()
			if err != nil {
				return err
			}
			v.ObsoletePrevLogNum = n

		case tagColumnFamily, tagColumnFamilyAdd, tagColumnFamilyDrop, tagMaxColumnFamily:
			return errors.New("column families are not supported")

		default:
			return errCorruptManifest
		}
	}
	return nil
}

// Encode encodes an edit to the specified writer.
func (v *VersionEdit) Encode(w io.Writer) error {
	e := versionEditEncoder{new(bytes.Buffer)}

	if v.ComparerName != "" {
		e.writeUvarint(tagComparator)
		e.writeString(v.ComparerName)
	}
	if v.MinUnflushedLogNum != 0 {
		e.writeUvarint(tagLogNumber)
		e.writeUvarint(uint64(v.MinUnflushedLogNum))
	}
	if v.ObsoletePrevLogNum != 0 {
		e.writeUvarint(tagPrevLogNumber)
		e.writeUvarint(v.ObsoletePrevLogNum)
	}
	if v.NextFileNum != 0 {
		e.writeUvarint(tagNextFileNumber)
		e.writeUvarint(uint64(v.NextFileNum))
	}
	// RocksDB requires LastSeqNum to be encoded for the first MANIFEST entry,
	// even though its value is zero. We detect this by encoding LastSeqNum when
	// ComparerName is set.
	if v.LastSeqNum != 0 || v.ComparerName != "" {
		e.writeUvarint(tagLastSequence)
		e.writeUvarint(v.LastSeqNum)
	}
	for x := range v.DeletedFiles {
		e.writeUvarint(tagDeletedFile)
		e.writeUvarint(uint64(x.Level))
		e.writeUvarint(uint64(x.FileNum))
	}
	for _, x := range v.NewFiles {
		var customFields bool
		if x.Meta.MarkedForCompaction || x.Meta.CreationTime != 0 {
			customFields = true
			e.writeUvarint(tagNewFile4)
		} else {
			e.writeUvarint(tagNewFile2)
		}
		e.writeUvarint(uint64(x.Level))
		e.writeUvarint(uint64(x.Meta.FileNum))
		e.writeUvarint(x.Meta.Size)
		e.writeKey(x.Meta.Smallest)
		e.writeKey(x.Meta.Largest)
		e.writeUvarint(x.Meta.SmallestSeqNum)
		e.writeUvarint(x.Meta.LargestSeqNum)
		if customFields {
			if x.Meta.CreationTime != 0 {
				e.writeUvarint(customTagCreationTime)
				var buf [binary.MaxVarintLen64]byte
				n := binary.PutUvarint(buf[:], uint64(x.Meta.CreationTime))
				e.writeBytes(buf[:n])
			}
			if x.Meta.MarkedForCompaction {
				e.writeUvarint(customTagNeedsCompaction)
				e.writeBytes([]byte{1})
			}
			e.writeUvarint(customTagTerminate)
		}
	}
	_, err := w.Write(e.Bytes())
	return err
}

type versionEditDecoder struct {
	byteReader
}

func (d versionEditDecoder) readBytes() ([]byte, error) {
	n, err := d.readUvarint()
	if err != nil {
		return nil, err
	}
	s := make([]byte, n)
	_, err = io.ReadFull(d, s)
	if err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, errCorruptManifest
		}
		return nil, err
	}
	return s, nil
}

func (d versionEditDecoder) readLevel() (int, error) {
	u, err := d.readUvarint()
	if err != nil {
		return 0, err
	}
	if u >= NumLevels {
		return 0, errCorruptManifest
	}
	return int(u), nil
}

func (d versionEditDecoder) readFileNum() (base.FileNum, error) {
	u, err := d.readUvarint()
	if err != nil {
		return 0, err
	}
	return base.FileNum(u), nil
}

func (d versionEditDecoder) readUvarint() (uint64, error) {
	u, err := binary.ReadUvarint(d)
	if err != nil {
		if err == io.EOF {
			return 0, errCorruptManifest
		}
		return 0, err
	}
	return u, nil
}

type versionEditEncoder struct {
	*bytes.Buffer
}

func (e versionEditEncoder) writeBytes(p []byte) {
	e.writeUvarint(uint64(len(p)))
	e.Write(p)
}

func (e versionEditEncoder) writeKey(k InternalKey) {
	e.writeUvarint(uint64(k.Size()))
	e.Write(k.UserKey)
	buf := k.EncodeTrailer()
	e.Write(buf[:])
}

func (e versionEditEncoder) writeString(s string) {
	e.writeUvarint(uint64(len(s)))
	e.WriteString(s)
}

func (e versionEditEncoder) writeUvarint(u uint64) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], u)
	e.Write(buf[:n])
}

// BulkVersionEdit summarizes the files added and deleted from a set of version
// edits.
type BulkVersionEdit struct {
	Added   [NumLevels][]*FileMetadata
	Deleted [NumLevels]map[base.FileNum]bool
}

// Accumulate adds the file addition and deletions in the specified version
// edit to the bulk edit's internal state.
func (b *BulkVersionEdit) Accumulate(ve *VersionEdit) {
	for df := range ve.DeletedFiles {
		dmap := b.Deleted[df.Level]
		if dmap == nil {
			dmap = make(map[base.FileNum]bool)
			b.Deleted[df.Level] = dmap
		}
		dmap[df.FileNum] = true
	}

	for _, nf := range ve.NewFiles {
		// A new file should not have been deleted in this or a preceding
		// VersionEdit at the same level (though files can move across levels).
		if dmap := b.Deleted[nf.Level]; dmap != nil {
			if _, ok := dmap[nf.Meta.FileNum]; ok {
				panic(fmt.Sprintf("file deleted %d before it was inserted\n", nf.Meta.FileNum))
			}
		}
		b.Added[nf.Level] = append(b.Added[nf.Level], nf.Meta)
	}
}

// Apply applies the delta b to the current version to produce a new
// version. The new version is consistent with respect to the comparer cmp.
//
// curr may be nil, which is equivalent to a pointer to a zero version.
//
// On success, a map of zombie files containing the file numbers and sizes of
// deleted files is returned. These files are considered zombies because they
// are no longer referenced by the returned Version, but cannot be deleted from
// disk as they are still in use by the incoming Version.
func (b *BulkVersionEdit) Apply(
	curr *Version, cmp Compare, formatKey base.FormatKey, flushSplitBytes int64,
) (_ *Version, zombies map[base.FileNum]uint64, _ error) {
	addZombie := func(fileNum base.FileNum, size uint64) {
		if zombies == nil {
			zombies = make(map[base.FileNum]uint64)
		}
		zombies[fileNum] = size
	}
	// The remove zombie function is used to handle tables that are moved from
	// one level to another during a version edit (i.e. a "move" compaction).
	removeZombie := func(fileNum base.FileNum) {
		if zombies != nil {
			delete(zombies, fileNum)
		}
	}

	v := new(Version)
	for level := range v.Levels {
		if len(b.Added[level]) == 0 && len(b.Deleted[level]) == 0 {
			// There are no edits on this level.
			if level == 0 {
				// Initialize L0Sublevels.
				if curr == nil || curr.L0Sublevels == nil {
					if err := v.InitL0Sublevels(cmp, formatKey, flushSplitBytes); err != nil {
						return nil, nil, errors.Wrap(err, "pebble: internal error")
					}
				} else {
					v.L0Sublevels = curr.L0Sublevels
				}
			}
			if curr == nil {
				continue
			}
			files := curr.Levels[level]
			v.Levels[level] = files
			// We still have to bump the ref count for all files.
			for i := range files {
				atomic.AddInt32(&files[i].refs, 1)
			}
			continue
		}

		// Some edits on this level.
		var currFiles []*FileMetadata
		if curr != nil {
			currFiles = curr.Levels[level]
		}
		addedFiles := b.Added[level]
		deletedMap := b.Deleted[level]
		n := len(currFiles) + len(addedFiles)
		if n == 0 {
			return nil, nil, errors.Errorf(
				"pebble: internal error: No current or added files but have deleted files: %d",
				errors.Safe(len(deletedMap)))
		}
		v.Levels[level] = make([]*FileMetadata, 0, n)
		// We have 2 lists of files, currFiles and addedFiles either of which (but not both) can
		// be empty.
		// - currFiles is internally consistent, since it comes from curr.
		// - addedFiles is not necessarily internally consistent, since it does not reflect deletions
		//   in deletedMap (since b could have accumulated multiple VersionEdits, the same file can
		//   be added and deleted). And we can delay checking consistency of it until we merge
		//   currFiles, addedFiles and deletedMap.
		if level == 0 {
			// - Note that any ingested single sequence number (ssn) file contained inside a multi-sequence
			//   number (msn) file must have been added before the latter. So it is not possible for
			//   an ssn file to be in addedFiles and its corresponding msn file to be in currFiles,
			//   but the reverse is possible. So for consistency checking we may need to look back
			//   into currFiles for ssn files that overlap with an msn file in addedFiles.
			// - The previous bullet does not hold for sequence number 0 files that can be added
			//   later. See the CheckOrdering func in version.go for a detailed explanation.
			//   Due to these files, the LargestSeqNums may not be increasing across the slice formed by
			//   concatenating addedFiles and currFiles.
			// - Instead of constructing a custom variant of the CheckOrdering logic, that is aware
			//   of the 2 slices, we observe that the number of L0 files is small so we can afford
			//   to repeat the full check on the combined slices (and CheckOrdering only does
			//   sequence num comparisons and not expensive key comparisons).
			for _, ff := range [2][]*FileMetadata{currFiles, addedFiles} {
				for i := range ff {
					f := ff[i]
					if deletedMap[f.FileNum] {
						addZombie(f.FileNum, f.Size)
						continue
					}
					atomic.AddInt32(&f.refs, 1)
					v.Levels[level] = append(v.Levels[level], f)
				}
			}
			SortBySeqNum(v.Levels[level])
			if err := v.InitL0Sublevels(cmp, formatKey, flushSplitBytes); err != nil {
				return nil, nil, errors.Wrap(err, "pebble: internal error")
			}
			if err := CheckOrdering(cmp, formatKey, Level(0), v.Levels[level].Iter()); err != nil {
				return nil, nil, errors.Wrap(err, "pebble: internal error")
			}
			continue
		}

		// level > 0.
		// - Sort the addedFiles in increasing order of the smallest key.
		// - In a large db, addedFiles is expected to be much smaller than currFiles, so we
		//   want to avoid comparing the addedFiles with each file in currFile.
		SortBySmallest(addedFiles, cmp)
		for i := range addedFiles {
			f := addedFiles[i]
			if deletedMap[f.FileNum] {
				addZombie(f.FileNum, f.Size)
				continue
			}
			removeZombie(f.FileNum)
			atomic.AddInt32(&f.refs, 1)
			// We need to add f. Find the first file in currFiles such that its smallest key
			// is > f.Largest. This file (if it is kept) will be the immediate successor of f.
			// The files in currFiles before this file (if they are kept) will precede f.
			//
			// Typically all the added files in a VersionEdit are from a single compaction
			// output, so after we add the first file, the subsequent files should have keys
			// preceding currFiles[0], so we could fast-path by first testing for that case before
			// calling sort.Search().
			j := sort.Search(len(currFiles), func(i int) bool {
				return base.InternalCompare(cmp, currFiles[i].Smallest, f.Largest) > 0
			})
			// Add the preceding files from currFiles.
			for k := 0; k < j; k++ {
				cf := currFiles[k]
				if deletedMap[cf.FileNum] {
					addZombie(cf.FileNum, cf.Size)
					continue
				}
				removeZombie(cf.FileNum)
				atomic.AddInt32(&cf.refs, 1)
				v.Levels[level] = append(v.Levels[level], cf)
			}
			currFiles = currFiles[j:]
			numFiles := len(v.Levels[level])
			if numFiles > 0 {
				// We expect k to typically be large, and we can avoid doing consistency
				// checks of the files within that set of k, since they are already mutually
				// consistent.
				//
				// Check the consistency of f with its predecessor in v.Files[level]. Note that
				// its predecessor either came from currFiles or addedFiles, and both are ones
				// which we need to check against f for consistency (since we have not checked
				// addedFiles for internal consistency).
				if base.InternalCompare(cmp, v.Levels[level][numFiles-1].Largest, f.Smallest) >= 0 {
					cf := v.Levels[level][numFiles-1]
					return nil, nil, errors.Errorf(
						"pebble: internal error: L%d files %s and %s have overlapping ranges: [%s-%s] vs [%s-%s]",
						errors.Safe(level), errors.Safe(cf.FileNum), errors.Safe(f.FileNum),
						cf.Smallest.Pretty(formatKey), cf.Largest.Pretty(formatKey),
						f.Smallest.Pretty(formatKey), f.Largest.Pretty(formatKey))
				}
			}
			v.Levels[level] = append(v.Levels[level], f)
		}
		// Add any remaining files in currFiles that are after all the added files.
		for i := range currFiles {
			f := currFiles[i]
			if deletedMap[f.FileNum] {
				addZombie(f.FileNum, f.Size)
				continue
			}
			removeZombie(f.FileNum)
			atomic.AddInt32(&f.refs, 1)
			v.Levels[level] = append(v.Levels[level], f)
		}
	}
	return v, zombies, nil
}
