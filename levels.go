/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"time"

	"github.com/coocood/badger/epoch"
	"github.com/coocood/badger/options"
	"github.com/coocood/badger/protos"
	"github.com/coocood/badger/table"
	"github.com/coocood/badger/y"
	"github.com/ncw/directio"
	"github.com/ngaut/log"
	"github.com/pingcap/errors"
	"golang.org/x/time/rate"
)

type levelsController struct {
	nextFileID uint64 // Atomic

	// The following are initialized once and const.
	resourceMgr *epoch.ResourceManager
	levels      []*levelHandler
	kv          *DB

	cstatus compactStatus

	opt options.TableBuilderOptions
}

var (
	// This is for getting timings between stalls.
	lastUnstalled time.Time
)

// revertToManifest checks that all necessary table files exist and removes all table files not
// referenced by the manifest.  idMap is a set of table file id's that were read from the directory
// listing.
func revertToManifest(kv *DB, mf *Manifest, idMap map[uint64]struct{}) error {
	// 1. Check all files in manifest exist.
	for id := range mf.Tables {
		if _, ok := idMap[id]; !ok {
			return fmt.Errorf("file does not exist for table %d", id)
		}
	}

	// 2. Delete files that shouldn't exist.
	for id := range idMap {
		if _, ok := mf.Tables[id]; !ok {
			log.Infof("Table file %d not referenced in MANIFEST", id)
			filename := table.NewFilename(id, kv.opt.Dir)
			if err := os.Remove(filename); err != nil {
				return y.Wrapf(err, "While removing table %d", id)
			}
		}
	}

	return nil
}

func newLevelsController(kv *DB, mf *Manifest, mgr *epoch.ResourceManager, opt options.TableBuilderOptions) (*levelsController, error) {
	y.Assert(kv.opt.NumLevelZeroTablesStall > kv.opt.NumLevelZeroTables)
	s := &levelsController{
		kv:          kv,
		levels:      make([]*levelHandler, kv.opt.TableBuilderOptions.MaxLevels),
		opt:         opt,
		resourceMgr: mgr,
	}
	s.cstatus.levels = make([]*levelCompactStatus, kv.opt.TableBuilderOptions.MaxLevels)

	for i := 0; i < kv.opt.TableBuilderOptions.MaxLevels; i++ {
		s.levels[i] = newLevelHandler(kv, i)
		if i == 0 {
			// Do nothing.
		} else if i == 1 {
			// Level 1 probably shouldn't be too much bigger than level 0.
			s.levels[i].maxTotalSize = kv.opt.LevelOneSize
		} else {
			s.levels[i].maxTotalSize = s.levels[i-1].maxTotalSize * int64(kv.opt.TableBuilderOptions.LevelSizeMultiplier)
		}
		s.cstatus.levels[i] = new(levelCompactStatus)
	}

	// Compare manifest against directory, check for existent/non-existent files, and remove.
	if err := revertToManifest(kv, mf, getIDMap(kv.opt.Dir)); err != nil {
		return nil, err
	}

	// Some files may be deleted. Let's reload.
	tables := make([][]*table.Table, kv.opt.TableBuilderOptions.MaxLevels)
	var maxFileID uint64
	for fileID, tableManifest := range mf.Tables {
		fname := table.NewFilename(fileID, kv.opt.Dir)
		var flags uint32 = y.Sync
		if kv.opt.ReadOnly {
			flags |= y.ReadOnly
		}

		t, err := table.OpenTable(fname, tableManifest.Compression, kv.blockCache, kv.indexCache)
		if err != nil {
			closeAllTables(tables)
			return nil, errors.Wrapf(err, "Opening table: %q", fname)
		}

		level := tableManifest.Level
		tables[level] = append(tables[level], t)

		if fileID > maxFileID {
			maxFileID = fileID
		}
	}
	s.nextFileID = maxFileID + 1
	for i, tbls := range tables {
		s.levels[i].initTables(tbls)
	}

	// Make sure key ranges do not overlap etc.
	if err := s.validate(); err != nil {
		_ = s.cleanupLevels()
		return nil, errors.Wrap(err, "Level validation")
	}

	// Sync directory (because we have at least removed some files, or previously created the
	// manifest file).
	if err := syncDir(kv.opt.Dir); err != nil {
		_ = s.close()
		return nil, err
	}

	return s, nil
}

// Closes the tables, for cleanup in newLevelsController.  (We Close() instead of using DecrRef()
// because that would delete the underlying files.)  We ignore errors, which is OK because tables
// are read-only.
func closeAllTables(tables [][]*table.Table) {
	for _, tableSlice := range tables {
		for _, table := range tableSlice {
			_ = table.Close()
		}
	}
}

func (lc *levelsController) cleanupLevels() error {
	var firstErr error
	for _, l := range lc.levels {
		if err := l.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (lc *levelsController) startCompact(c *y.Closer) {
	n := lc.kv.opt.NumCompactors
	c.AddRunning(n - 1)
	for i := 0; i < n; i++ {
		// The first half compaction workers take level as priority, others take score
		// as priority.
		go lc.runWorker(c, i*2 >= n)
	}
}

func (lc *levelsController) runWorker(c *y.Closer, scorePriority bool) {
	defer c.Done()
	if lc.kv.opt.DoNotCompact {
		return
	}

	time.Sleep(time.Duration(rand.Int31n(1000)) * time.Millisecond)

	for {
		guard := lc.resourceMgr.Acquire()
		prios := lc.pickCompactLevels()
		if scorePriority {
			sort.Slice(prios, func(i, j int) bool {
				return prios[i].score > prios[j].score
			})
		}
		var didCompact bool
		for _, p := range prios {
			// TODO: Handle error.
			didCompact, _ = lc.doCompact(p, guard)
			if didCompact {
				break
			}
		}
		guard.Done()
		waitDur := time.Second * 3
		if didCompact {
			waitDur /= 10
		}
		timer := time.NewTimer(waitDur)
		select {
		case <-c.HasBeenClosed():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Returns true if level zero may be compacted, without accounting for compactions that already
// might be happening.
func (lc *levelsController) isL0Compactable() bool {
	return lc.levels[0].numTables() >= lc.kv.opt.NumLevelZeroTables
}

// Returns true if the non-zero level may be compacted.  deltaSize provides the size of the tables
// which are currently being compacted so that we treat them as already having started being
// compacted (because they have been, yet their size is already counted in getTotalSize).
func (l *levelHandler) isCompactable(deltaSize int64) bool {
	return l.getTotalSize() >= l.maxTotalSize+deltaSize
}

type compactionPriority struct {
	level int
	score float64
}

// pickCompactLevel determines which level to compact.
// Based on: https://github.com/facebook/rocksdb/wiki/Leveled-Compaction
func (lc *levelsController) pickCompactLevels() (prios []compactionPriority) {
	// This function must use identical criteria for guaranteeing compaction's progress that
	// addLevel0Table uses.

	// cstatus is checked to see if level 0's tables are already being compacted
	if !lc.cstatus.overlapsWith(0, infRange) && lc.isL0Compactable() {
		pri := compactionPriority{
			level: 0,
			score: float64(lc.levels[0].numTables()) / float64(lc.kv.opt.NumLevelZeroTables),
		}
		prios = append(prios, pri)
	}

	// now calcalute scores from level 1
	for levelNum := 1; levelNum < len(lc.levels); levelNum++ {
		// Don't consider those tables that are already being compacted right now.
		deltaSize := lc.cstatus.deltaSize(levelNum)

		l := lc.levels[levelNum]
		if l.isCompactable(deltaSize) {
			pri := compactionPriority{
				level: levelNum,
				score: float64(l.getTotalSize()-deltaSize) / float64(l.maxTotalSize),
			}
			prios = append(prios, pri)
		}
	}
	// We used to sort compaction priorities based on the score. But, we
	// decided to compact based on the level, not the priority. So, upper
	// levels (level 0, level 1, etc) always get compacted first, before the
	// lower levels -- this allows us to avoid stalls.
	return prios
}

func (lc *levelsController) hasOverlapTable(cd *compactDef) bool {
	kr := getKeyRange(cd.top)
	for i := cd.nextLevel.level + 1; i < len(lc.levels); i++ {
		lh := lc.levels[i]
		lh.RLock()
		left, right := lh.overlappingTables(levelHandlerRLocked{}, kr)
		lh.RUnlock()
		if right-left > 0 {
			return true
		}
	}
	return false
}

type DiscardStats struct {
	numSkips     int64
	skippedBytes int64
	ptrs         []blobPointer
}

func (ds *DiscardStats) collect(vs y.ValueStruct) {
	if vs.Meta&bitValuePointer > 0 {
		var bp blobPointer
		bp.decode(vs.Value)
		ds.ptrs = append(ds.ptrs, bp)
		ds.skippedBytes += int64(bp.length)
	}
	ds.numSkips++
}

func (ds *DiscardStats) String() string {
	return fmt.Sprintf("numSkips:%d, skippedBytes:%d", ds.numSkips, ds.skippedBytes)
}

func shouldFinishFile(key, lastKey y.Key, guard *Guard, currentSize, maxSize int64) bool {
	if lastKey.IsEmpty() {
		return false
	}
	if guard != nil {
		if !bytes.HasPrefix(key.UserKey, guard.Prefix) {
			return true
		}
		if !matchGuard(key.UserKey, lastKey.UserKey, guard) {
			if maxSize > guard.MinSize {
				maxSize = guard.MinSize
			}
		}
	}
	return currentSize > maxSize
}

func matchGuard(key, lastKey []byte, guard *Guard) bool {
	if len(lastKey) < guard.MatchLen {
		return false
	}
	return bytes.HasPrefix(key, lastKey[:guard.MatchLen])
}

func searchGuard(key []byte, guards []Guard) *Guard {
	var maxMatchGuard *Guard
	for i := range guards {
		guard := &guards[i]
		if bytes.HasPrefix(key, guard.Prefix) {
			if maxMatchGuard == nil || len(guard.Prefix) > len(maxMatchGuard.Prefix) {
				maxMatchGuard = guard
			}
		}
	}
	return maxMatchGuard
}

func overSkipTables(key y.Key, skippedTables []*table.Table) (newSkippedTables []*table.Table, over bool) {
	var i int
	for i < len(skippedTables) {
		t := skippedTables[i]
		if key.Compare(t.Biggest()) > 0 {
			i++
		} else {
			break
		}
	}
	return skippedTables[i:], i > 0
}

// compactBuildTables merge topTables and botTables to form a list of new tables.
func (lc *levelsController) compactBuildTables(level int, cd *compactDef,
	limiter *rate.Limiter, splitHints []y.Key) (newTables []*table.Table, err error) {
	topTables := cd.top
	botTables := cd.bot

	hasOverlap := lc.hasOverlapTable(cd)
	log.Infof("Key range overlaps with lower levels: %v", hasOverlap)

	// Try to collect stats so that we can inform value log about GC. That would help us find which
	// value log file should be GCed.
	discardStats := &DiscardStats{}

	// Create iterators across all the tables involved first.
	var iters []y.Iterator
	if level == 0 {
		iters = appendIteratorsReversed(iters, topTables, false)
	} else {
		iters = []y.Iterator{table.NewConcatIterator(topTables, false)}
	}

	// Next level has level>=1 and we can use ConcatIterator as key ranges do not overlap.
	iters = append(iters, table.NewConcatIterator(botTables, false))
	it := table.NewMergeIterator(iters, false)

	it.Rewind()

	// Pick up the currently pending transactions' min readTs, so we can discard versions below this
	// readTs. We should never discard any versions starting from above this timestamp, because that
	// would affect the snapshot view guarantee provided by transactions.
	safeTs := lc.kv.getCompactSafeTs()

	var filter CompactionFilter
	var guards []Guard
	if lc.kv.opt.CompactionFilterFactory != nil {
		filter = lc.kv.opt.CompactionFilterFactory(level+1, cd.smallest().UserKey, cd.biggest().UserKey)
		guards = filter.Guards()
	}
	skippedTbls := cd.skippedTbls

	var lastKey, skipKey y.Key
	var builder *table.Builder
	var bytesRead, bytesWrite, numRead, numWrite int
	for it.Valid() {
		fileID := lc.reserveFileID()
		filename := table.NewFilename(fileID, lc.kv.opt.Dir)
		var fd *os.File
		fd, err = directio.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			return
		}
		if builder == nil {
			builder = table.NewTableBuilder(fd, limiter, cd.nextLevel.level, lc.opt)
		} else {
			builder.Reset(fd)
		}
		lastKey.Reset()
		guard := searchGuard(it.Key().UserKey, guards)
		for ; it.Valid(); it.Next() {
			numRead++
			vs := it.Value()
			key := it.Key()
			kvSize := int(vs.EncodedSize()) + key.Len()
			bytesRead += kvSize
			// See if we need to skip this key.
			if !skipKey.IsEmpty() {
				if key.SameUserKey(skipKey) {
					discardStats.collect(vs)
					continue
				} else {
					skipKey.Reset()
				}
			}
			if !key.SameUserKey(lastKey) {
				// Only break if we are on a different key, and have reached capacity. We want
				// to ensure that all versions of the key are stored in the same sstable, and
				// not divided across multiple tables at the same level.
				if len(skippedTbls) > 0 {
					var over bool
					skippedTbls, over = overSkipTables(key, skippedTbls)
					if over && !builder.Empty() {
						break
					}
				}
				if shouldFinishFile(key, lastKey, guard, int64(builder.EstimateSize()), lc.kv.opt.MaxTableSize) {
					break
				}
				if len(splitHints) != 0 && key.Compare(splitHints[0]) >= 0 {
					splitHints = splitHints[1:]
					for len(splitHints) > 0 && key.Compare(splitHints[0]) >= 0 {
						splitHints = splitHints[1:]
					}
					break
				}
				lastKey.Copy(key)
			}

			// Only consider the versions which are below the minReadTs, otherwise, we might end up discarding the
			// only valid version for a running transaction.
			if key.Version <= safeTs {
				// key is the latest readable version of this key, so we simply discard all the rest of the versions.
				skipKey.Copy(key)

				if isDeleted(vs.Meta) {
					// If this key range has overlap with lower levels, then keep the deletion
					// marker with the latest version, discarding the rest. We have set skipKey,
					// so the following key versions would be skipped. Otherwise discard the deletion marker.
					if !hasOverlap {
						continue
					}
				} else if filter != nil {
					switch filter.Filter(key.UserKey, vs.Value, vs.UserMeta) {
					case DecisionMarkTombstone:
						discardStats.collect(vs)
						if hasOverlap {
							// There may have ole versions for this key, so convert to delete tombstone.
							builder.Add(key, y.ValueStruct{Meta: bitDelete})
						}
						continue
					case DecisionDrop:
						discardStats.collect(vs)
						continue
					case DecisionKeep:
					}
				}
			}
			builder.Add(key, vs)
			numWrite++
			bytesWrite += kvSize
		}
		// It was true that it.Valid() at least once in the loop above, which means we
		// called Add() at least once, and builder is not Empty().
		if err = builder.Finish(); err != nil {
			return
		}
		fd.Close()
		var tbl *table.Table
		tbl, err = table.OpenTable(filename, lc.opt.CompressionPerLevel[cd.nextLevel.level], lc.kv.blockCache, lc.kv.indexCache)
		if err != nil {
			return
		}
		if tbl.Smallest().IsEmpty() {
			tbl.Delete()
		} else {
			newTables = append(newTables, tbl)
		}
	}

	stats := &y.CompactionStats{
		KeysRead:     numRead,
		BytesRead:    bytesRead,
		KeysWrite:    numWrite,
		BytesWrite:   bytesWrite,
		KeysDiscard:  int(discardStats.numSkips),
		BytesDiscard: int(discardStats.skippedBytes),
	}
	cd.nextLevel.metrics.UpdateCompactionStats(stats)
	// Ensure created files' directory entries are visible.  We don't mind the extra latency
	// from not doing this ASAP after all file creation has finished because this is a
	// background operation.
	err = syncDir(lc.kv.opt.Dir)
	if err != nil {
		log.Error(err)
		return
	}
	sortTables(newTables)
	log.Infof("Discard stats: %s", discardStats)
	if len(discardStats.ptrs) > 0 {
		lc.kv.blobManger.discardCh <- discardStats
	}
	return
}

func buildChangeSet(cd *compactDef, newTables []*table.Table) protos.ManifestChangeSet {
	changes := []*protos.ManifestChange{}
	for _, table := range newTables {
		changes = append(changes,
			newCreateChange(table.ID(), cd.nextLevel.level, table.CompressionType()))
	}
	for _, table := range cd.top {
		changes = append(changes, newDeleteChange(table.ID()))
	}
	for _, table := range cd.bot {
		changes = append(changes, newDeleteChange(table.ID()))
	}
	return protos.ManifestChangeSet{Changes: changes}
}

type compactDef struct {
	thisLevel *levelHandler
	nextLevel *levelHandler

	top []*table.Table
	bot []*table.Table

	skippedTbls []*table.Table

	thisRange keyRange
	nextRange keyRange

	topSize     int64
	topLeftIdx  int
	topRightIdx int
	botSize     int64
	botLeftIdx  int
	botRightIdx int
}

func (cd *compactDef) String() string {
	return fmt.Sprintf("%d top:[%d:%d](%d), bot:[%d:%d](%d), skip:%d, write_amp:%.2f",
		cd.thisLevel.level, cd.topLeftIdx, cd.topRightIdx, cd.topSize,
		cd.botLeftIdx, cd.botRightIdx, cd.botSize, len(cd.skippedTbls), float64(cd.topSize+cd.botSize)/float64(cd.topSize))
}

func (cd *compactDef) lockLevels() {
	cd.thisLevel.RLock()
	cd.nextLevel.RLock()
}

func (cd *compactDef) unlockLevels() {
	cd.nextLevel.RUnlock()
	cd.thisLevel.RUnlock()
}

func (cd *compactDef) smallest() y.Key {
	if len(cd.bot) > 0 && cd.nextRange.left.Compare(cd.thisRange.left) < 0 {
		return cd.nextRange.left
	}
	return cd.thisRange.left
}

func (cd *compactDef) biggest() y.Key {
	if len(cd.bot) > 0 && cd.nextRange.right.Compare(cd.thisRange.right) > 0 {
		return cd.nextRange.right
	}
	return cd.thisRange.right
}

func (cd *compactDef) markTablesCompacting() {
	for _, tbl := range cd.top {
		tbl.MarkCompacting(true)
	}
	for _, tbl := range cd.bot {
		tbl.MarkCompacting(true)
	}
	for _, tbl := range cd.skippedTbls {
		tbl.MarkCompacting(true)
	}
}

func (lc *levelsController) fillTablesL0(cd *compactDef) bool {
	cd.lockLevels()
	defer cd.unlockLevels()

	if len(cd.thisLevel.tables) == 0 {
		return false
	}

	cd.top = make([]*table.Table, len(cd.thisLevel.tables))
	copy(cd.top, cd.thisLevel.tables)
	for _, t := range cd.top {
		cd.topSize += t.Size()
	}
	cd.topRightIdx = len(cd.top)

	cd.thisRange = infRange

	kr := getKeyRange(cd.top)
	left, right := cd.nextLevel.overlappingTables(levelHandlerRLocked{}, kr)
	overlappingTables := cd.nextLevel.tables[left:right]
	cd.botLeftIdx = left
	cd.botRightIdx = right
	lc.fillBottomTables(cd, overlappingTables)
	for _, t := range cd.bot {
		cd.botSize += t.Size()
	}

	if len(overlappingTables) == 0 { // the bottom-most level
		cd.nextRange = kr
	} else {
		cd.nextRange = getKeyRange(overlappingTables)
	}

	if !lc.cstatus.compareAndAdd(thisAndNextLevelRLocked{}, *cd) {
		return false
	}

	return true
}

const minSkippedTableSize = 1024 * 1024

func (lc *levelsController) fillBottomTables(cd *compactDef, overlappingTables []*table.Table) {
	for _, t := range overlappingTables {
		// If none of the top tables contains the range in an overlapping bottom table,
		// we can skip it during compaction to reduce write amplification.
		var added bool
		for _, topTbl := range cd.top {
			if topTbl.HasOverlap(t.Smallest(), t.Biggest(), true) {
				cd.bot = append(cd.bot, t)
				added = true
				break
			}
		}
		if !added {
			if t.Size() >= minSkippedTableSize {
				// We need to limit the minimum size of the table to be skipped,
				// otherwise the number of tables in a level will keep growing
				// until we meet too many open files error.
				cd.skippedTbls = append(cd.skippedTbls, t)
			} else {
				cd.bot = append(cd.bot, t)
			}
		}
	}
}

const maxCompactionExpandSize = 1 << 30 // 1GB

func (lc *levelsController) fillTables(cd *compactDef) bool {
	cd.lockLevels()
	defer cd.unlockLevels()

	if len(cd.thisLevel.tables) == 0 {
		return false
	}
	this := make([]*table.Table, len(cd.thisLevel.tables))
	copy(this, cd.thisLevel.tables)
	next := make([]*table.Table, len(cd.nextLevel.tables))
	copy(next, cd.nextLevel.tables)

	// First pick one table has max topSize/bottomSize ratio.
	var candidateRatio float64
	for i, t := range this {
		if lc.isCompacting(cd.thisLevel.level, t) {
			continue
		}
		left, right := getTablesInRange(next, t.Smallest(), t.Biggest())
		if lc.isCompacting(cd.nextLevel.level, next[left:right]...) {
			continue
		}
		botSize := sumTableSize(next[left:right])
		ratio := calcRatio(t.Size(), botSize)
		if ratio > candidateRatio {
			candidateRatio = ratio
			cd.topLeftIdx = i
			cd.topRightIdx = i + 1
			cd.top = this[cd.topLeftIdx:cd.topRightIdx:cd.topRightIdx]
			cd.topSize = t.Size()
			cd.botLeftIdx = left
			cd.botRightIdx = right
			cd.botSize = botSize
		}
	}
	if len(cd.top) == 0 {
		return false
	}
	bots := next[cd.botLeftIdx:cd.botRightIdx:cd.botRightIdx]
	// Expand to left to include more tops as long as the ratio doesn't decrease and the total size
	// do not exceeds maxCompactionExpandSize.
	for i := cd.topLeftIdx - 1; i >= 0; i-- {
		t := this[i]
		if lc.isCompacting(cd.thisLevel.level, t) {
			break
		}
		left, right := getTablesInRange(next, t.Smallest(), t.Biggest())
		if right < cd.botLeftIdx {
			// A bottom table is skipped, we can compact in another run.
			break
		}
		if lc.isCompacting(cd.nextLevel.level, next[left:cd.botLeftIdx]...) {
			break
		}
		newTopSize := t.Size() + cd.topSize
		newBotSize := sumTableSize(next[left:cd.botLeftIdx]) + cd.botSize
		newRatio := calcRatio(newTopSize, newBotSize)
		if newRatio > candidateRatio && (newTopSize+newBotSize) < maxCompactionExpandSize {
			cd.top = append([]*table.Table{t}, cd.top...)
			cd.topLeftIdx--
			bots = append(next[left:cd.botLeftIdx:cd.botLeftIdx], bots...)
			cd.botLeftIdx = left
			cd.topSize = newTopSize
			cd.botSize = newBotSize
		} else {
			break
		}
	}
	// Expand to right to include more tops as long as the ratio doesn't decrease and the total size
	// do not exceeds maxCompactionExpandSize.
	for i := cd.topRightIdx; i < len(this); i++ {
		t := this[i]
		if lc.isCompacting(cd.thisLevel.level, t) {
			break
		}
		left, right := getTablesInRange(next, t.Smallest(), t.Biggest())
		if left > cd.botRightIdx {
			// A bottom table is skipped, we can compact in another run.
			break
		}
		if lc.isCompacting(cd.nextLevel.level, next[cd.botRightIdx:right]...) {
			break
		}
		newTopSize := t.Size() + cd.topSize
		newBotSize := sumTableSize(next[cd.botRightIdx:right]) + cd.botSize
		newRatio := calcRatio(newTopSize, newBotSize)
		if newRatio > candidateRatio && (newTopSize+newBotSize) < maxCompactionExpandSize {
			cd.top = append(cd.top, t)
			cd.topRightIdx++
			bots = append(bots, next[cd.botRightIdx:right]...)
			cd.botRightIdx = right
			cd.topSize = newTopSize
			cd.botSize = newBotSize
		} else {
			break
		}
	}
	cd.thisRange = keyRange{left: cd.top[0].Smallest(), right: cd.top[len(cd.top)-1].Biggest()}
	if len(bots) > 0 {
		cd.nextRange = keyRange{left: bots[0].Smallest(), right: bots[len(bots)-1].Biggest()}
	} else {
		cd.nextRange = cd.thisRange
	}
	lc.fillBottomTables(cd, bots)
	for _, t := range cd.skippedTbls {
		cd.botSize -= t.Size()
	}
	return lc.cstatus.compareAndAdd(thisAndNextLevelRLocked{}, *cd)
}

func sumTableSize(tables []*table.Table) int64 {
	var size int64
	for _, t := range tables {
		size += t.Size()
	}
	return size
}

func calcRatio(topSize, botSize int64) float64 {
	if botSize == 0 {
		return float64(topSize)
	}
	return float64(topSize) / float64(botSize)
}

func (lc *levelsController) isCompacting(level int, tables ...*table.Table) bool {
	if len(tables) == 0 {
		return false
	}
	kr := keyRange{
		left:  tables[0].Smallest(),
		right: tables[len(tables)-1].Biggest(),
	}
	y.Assert(!kr.left.IsEmpty())
	y.Assert(!kr.right.IsEmpty())
	return lc.cstatus.overlapsWith(level, kr)
}

func (lc *levelsController) runCompactDef(l int, cd *compactDef, limiter *rate.Limiter, guard *epoch.Guard) error {
	timeStart := time.Now()

	thisLevel := cd.thisLevel
	nextLevel := cd.nextLevel

	var newTables []*table.Table
	var changeSet protos.ManifestChangeSet
	var topMove bool
	defer func() {
		for _, tbl := range newTables {
			tbl.MarkCompacting(false)
		}
		for _, tbl := range cd.skippedTbls {
			tbl.MarkCompacting(false)
		}
	}()

	if l > 0 && len(cd.bot) == 0 && len(cd.skippedTbls) == 0 {
		// skip level 0, since it may has many table overlap with each other
		newTables = cd.top
		changeSet = protos.ManifestChangeSet{}
		for _, t := range newTables {
			changeSet.Changes = append(changeSet.Changes, newMoveDownChange(t.ID(), cd.nextLevel.level))
		}
		topMove = true
	} else {
		var err error
		newTables, err = lc.compactBuildTables(l, cd, limiter, nil)
		if err != nil {
			return err
		}
		changeSet = buildChangeSet(cd, newTables)
	}

	// We write to the manifest _before_ we delete files (and after we created files)
	if err := lc.kv.manifest.addChanges(changeSet.Changes, nil); err != nil {
		return err
	}

	// See comment earlier in this function about the ordering of these ops, and the order in which
	// we access levels when reading.
	nextLevel.replaceTables(newTables, cd, guard)
	thisLevel.deleteTables(cd.top, guard, topMove)

	// Note: For level 0, while doCompact is running, it is possible that new tables are added.
	// However, the tables are added only to the end, so it is ok to just delete the first table.

	log.Infof("LOG Compact %s, del %d tables, add %d tables, took %v",
		cd, len(cd.top)+len(cd.bot), len(newTables), time.Since(timeStart))
	return nil
}

// doCompact picks some table on level l and compacts it away to the next level.
func (lc *levelsController) doCompact(p compactionPriority, guard *epoch.Guard) (bool, error) {
	l := p.level
	y.Assert(l+1 < lc.kv.opt.TableBuilderOptions.MaxLevels) // Sanity check.

	cd := &compactDef{
		thisLevel: lc.levels[l],
		nextLevel: lc.levels[l+1],
	}

	log.Infof("Got compaction priority: %+v", p)

	// While picking tables to be compacted, both levels' tables are expected to
	// remain unchanged.
	if l == 0 {
		if !lc.fillTablesL0(cd) {
			log.Infof("fillTables failed for level: %d", l)
			return false, nil
		}
	} else {
		if !lc.fillTables(cd) {
			log.Infof("fillTables failed for level: %d", l)
			return false, nil
		}
	}
	defer lc.cstatus.delete(cd) // Remove the ranges from compaction status.

	log.Infof("Running compaction: %s", cd)
	if err := lc.runCompactDef(l, cd, lc.kv.limiter, guard); err != nil {
		// This compaction couldn't be done successfully.
		log.Infof("\tLOG Compact FAILED with error: %+v: %+v", err, cd)
		return false, err
	}

	log.Infof("Compaction Done for level: %d", cd.thisLevel.level)
	return true, nil
}

func (lc *levelsController) addLevel0Table(t *table.Table, head *protos.HeadInfo) error {
	// We update the manifest _before_ the table becomes part of a levelHandler, because at that
	// point it could get used in some compaction.  This ensures the manifest file gets updated in
	// the proper order. (That means this update happens before that of some compaction which
	// deletes the table.)
	err := lc.kv.manifest.addChanges([]*protos.ManifestChange{
		newCreateChange(t.ID(), 0, t.CompressionType()),
	}, head)
	if err != nil {
		return err
	}

	for !lc.levels[0].tryAddLevel0Table(t) {
		// Stall. Make sure all levels are healthy before we unstall.
		var timeStart time.Time
		{
			log.Warnf("STALLED STALLED STALLED: %v", time.Since(lastUnstalled))
			lc.cstatus.RLock()
			for i := 0; i < lc.kv.opt.TableBuilderOptions.MaxLevels; i++ {
				log.Warnf("level=%d. Status=%s Size=%d",
					i, lc.cstatus.levels[i].debug(), lc.levels[i].getTotalSize())
			}
			lc.cstatus.RUnlock()
			timeStart = time.Now()
		}
		// Before we unstall, we need to make sure that level 0 is healthy. Otherwise, we
		// will very quickly fill up level 0 again.
		for i := 0; ; i++ {
			// It's crucial that this behavior replicates pickCompactLevels' behavior in
			// computing compactability in order to guarantee progress.
			// Break the loop once L0 has enough space to accommodate new tables.
			if !lc.isL0Compactable() {
				break
			}
			time.Sleep(10 * time.Millisecond)
			if i%100 == 0 {
				prios := lc.pickCompactLevels()
				log.Warnf("Waiting to add level 0 table. Compaction priorities: %+v", prios)
				i = 0
			}
		}
		log.Infof("UNSTALLED UNSTALLED UNSTALLED UNSTALLED UNSTALLED UNSTALLED: %v",
			time.Since(timeStart))
		lastUnstalled = time.Now()
	}

	return nil
}

func (s *levelsController) close() error {
	err := s.cleanupLevels()
	return errors.Wrap(err, "levelsController.Close")
}

// get returns the found value if any. If not found, we return nil.
func (s *levelsController) get(key y.Key, keyHash uint64) y.ValueStruct {
	// It's important that we iterate the levels from 0 on upward.  The reason is, if we iterated
	// in opposite order, or in parallel (naively calling all the h.RLock() in some order) we could
	// read level L's tables post-compaction and level L+1's tables pre-compaction.  (If we do
	// parallelize this, we will need to call the h.RLock() function by increasing order of level
	// number.)
	start := time.Now()
	defer s.kv.metrics.LSMGetDuration.Observe(time.Since(start).Seconds())
	for _, h := range s.levels {
		vs := h.get(key, keyHash) // Calls h.RLock() and h.RUnlock().
		if vs.Valid() {
			return vs
		}
	}
	return y.ValueStruct{}
}

func (s *levelsController) multiGet(pairs []keyValuePair) {
	start := time.Now()
	for _, h := range s.levels {
		h.multiGet(pairs)
	}
	s.kv.metrics.LSMMultiGetDuration.Observe(time.Since(start).Seconds())
}

func appendIteratorsReversed(out []y.Iterator, th []*table.Table, reversed bool) []y.Iterator {
	for i := len(th) - 1; i >= 0; i-- {
		// This will increment the reference of the table handler.
		out = append(out, table.NewConcatIterator(th[i:i+1], reversed))
	}
	return out
}

// appendIterators appends iterators to an array of iterators, for merging.
// Note: This obtains references for the table handlers. Remember to close these iterators.
func (s *levelsController) appendIterators(
	iters []y.Iterator, opts *IteratorOptions) []y.Iterator {
	// Just like with get, it's important we iterate the levels from 0 on upward, to avoid missing
	// data when there's a compaction.
	for _, level := range s.levels {
		iters = level.appendIterators(iters, opts)
	}
	return iters
}

type TableInfo struct {
	ID    uint64
	Level int
	Left  []byte
	Right []byte
}

func (lc *levelsController) getTableInfo() (result []TableInfo) {
	for _, l := range lc.levels {
		for _, t := range l.tables {
			info := TableInfo{
				ID:    t.ID(),
				Level: l.level,
				Left:  t.Smallest().UserKey,
				Right: t.Biggest().UserKey,
			}
			result = append(result, info)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Level != result[j].Level {
			return result[i].Level < result[j].Level
		}
		return result[i].ID < result[j].ID
	})
	return
}
