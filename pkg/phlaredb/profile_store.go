package phlaredb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/runutil"
	"github.com/pkg/errors"
	"github.com/segmentio/parquet-go"
	"go.uber.org/atomic"

	"github.com/grafana/phlare/pkg/iter"
	phlaremodel "github.com/grafana/phlare/pkg/model"
	phlarecontext "github.com/grafana/phlare/pkg/phlare/context"
	"github.com/grafana/phlare/pkg/phlaredb/block"
	query "github.com/grafana/phlare/pkg/phlaredb/query"
	schemav1 "github.com/grafana/phlare/pkg/phlaredb/schemas/v1"
	"github.com/grafana/phlare/pkg/util/build"
)

type profileStore struct {
	slice []*schemav1.Profile
	size  atomic.Uint64
	lock  sync.RWMutex

	metrics *headMetrics

	index *profilesIndex

	persister schemav1.Persister[*schemav1.Profile]
	helper    storeHelper[*schemav1.Profile]

	logger log.Logger
	cfg    *ParquetConfig

	writer *parquet.GenericWriter[*schemav1.Profile]

	path        string
	rowsFlushed uint64

	rowGroups []*rowGroupOnDisk
}

func newProfileStore(phlarectx context.Context) *profileStore {
	var s = &profileStore{
		logger:    phlarecontext.Logger(phlarectx),
		metrics:   contextHeadMetrics(phlarectx),
		persister: &schemav1.ProfilePersister{},
		helper:    &profilesHelper{},
	}

	// Initialize writer on /dev/null
	// TODO: Reuse parquet.Writer beyond life time of the head.
	s.writer = parquet.NewGenericWriter[*schemav1.Profile](io.Discard, s.persister.Schema(),
		parquet.ColumnPageBuffers(parquet.NewFileBufferPool(os.TempDir(), "phlaredb-parquet-buffers*")),
		parquet.CreatedBy("github.com/grafana/phlare/", build.Version, build.Revision),
	)

	return s
}

func (s *profileStore) Name() string {
	return s.persister.Name()
}

func (s *profileStore) Size() uint64 {
	return s.size.Load()
}

// resets the store
func (s *profileStore) Init(path string, cfg *ParquetConfig) (err error) {
	// close previous iteration
	if err := s.Close(); err != nil {
		return err
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	// create index
	s.index, err = newProfileIndex(32, s.metrics)
	if err != nil {
		return err
	}

	s.path = path
	s.cfg = cfg

	s.slice = s.slice[:0]

	s.rowsFlushed = 0

	return nil
}

func (s *profileStore) Close() error {
	return nil
}

func (s *profileStore) RowGroups() (rowGroups []parquet.RowGroup) {

	rowGroups = make([]parquet.RowGroup, len(s.rowGroups))
	for pos := range rowGroups {
		rowGroups[pos] = s.rowGroups[pos]
	}
	return rowGroups
}

func (s *profileStore) profileSort(i, j int) bool {
	// first compare the labels, if they don't match return
	var (
		pI   = s.slice[i]
		pJ   = s.slice[j]
		lbsI = s.index.profilesPerFP[pI.SeriesFingerprint].lbs
		lbsJ = s.index.profilesPerFP[pJ.SeriesFingerprint].lbs
	)
	if cmp := phlaremodel.CompareLabelPairs(lbsI, lbsJ); cmp != 0 {
		return cmp < 0
	}

	// then compare timenanos, if they don't match return
	if pI.TimeNanos < pJ.TimeNanos {
		return true
	} else if pI.TimeNanos > pJ.TimeNanos {
		return false
	}

	// finally use ID as tie breaker
	return bytes.Compare(pI.ID[:], pJ.ID[:]) < 0

}

func (s *profileStore) Flush(ctx context.Context) (numRows uint64, numRowGroups uint64, err error) {
	if err := s.Close(); err != nil {
		return 0, 0, err
	}

	if err := s.cutRowGroup(); err != nil {
		return 0, 0, err
	}

	indexPath := filepath.Join(
		s.path,
		block.IndexFilename,
	)

	if err := s.index.writeTo(ctx, indexPath); err != nil {
		return 0, 0, err
	}

	parquetPath := filepath.Join(
		s.path,
		s.persister.Name()+block.ParquetSuffix,
	)

	numRows, numRowGroups, err = s.writeRowGroups(parquetPath, s.RowGroups())
	if err != nil {
		return 0, 0, err
	}

	// cleanup row groups, which need cleaning
	for _, rg := range s.rowGroups {
		if err := rg.Close(); err != nil {
			return 0, 0, err
		}
	}
	s.rowGroups = s.rowGroups[:0]

	return numRows, numRowGroups, nil
}

func (s *profileStore) prepareFile(path string) (closer io.Closer, err error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	s.writer.Reset(file)

	return file, err
}

// cutRowGroups gets called, when a patrticular row group has been finished and it will flush it to disk. The caller of cutRowGroups should be holding the write lock.
// TODO: write row groups asynchronously
func (s *profileStore) cutRowGroup() (err error) {
	// do nothing with empty buffer
	bufferRowNums := len(s.slice)
	if bufferRowNums == 0 {
		return nil
	}

	path := filepath.Join(
		s.path,
		fmt.Sprintf("%s.%d%s", s.persister.Name(), s.rowsFlushed, block.ParquetSuffix),
	)

	fileCloser, err := s.prepareFile(path)
	if err != nil {
		return err
	}
	defer runutil.CloseWithErrCapture(&err, fileCloser, "closing row group segment file")

	// order profiles properly
	sort.Slice(s.slice, s.profileSort)

	n, err := s.writer.Write(s.slice)
	if err != nil {
		return errors.Wrap(err, "write row group segments to disk")
	}

	if err := s.writer.Close(); err != nil {
		return errors.Wrap(err, "write row group segments to disk")
	}

	s.rowsFlushed += uint64(n)

	rowGroup, err := newRowGroupOnDisk(path)
	if err != nil {
		return err
	}
	s.rowGroups = append(s.rowGroups, rowGroup)

	// let index know about row group
	if err := s.index.cutRowGroup(s.slice, rowGroup); err != nil {
		return err
	}

	// reset slice and metrics
	s.slice = s.slice[:0]
	s.size.Store(0)

	return nil
}

func (s *profileStore) writeRowGroups(path string, rowGroups []parquet.RowGroup) (n uint64, numRowGroups uint64, err error) {
	fileCloser, err := s.prepareFile(path)
	if err != nil {
		return 0, 0, err
	}
	defer runutil.CloseWithErrCapture(&err, fileCloser, "closing parquet file")

	for rgN, rg := range rowGroups {
		level.Debug(s.logger).Log("msg", "writing row group", "path", path, "row_group_number", rgN, "rows", rg.NumRows())

		nInt64, err := s.writer.WriteRowGroup(rg)
		if err != nil {
			return 0, 0, err
		}
		n += uint64(nInt64)
		numRowGroups += 1
	}

	if err := s.writer.Close(); err != nil {
		return 0, 0, err
	}

	s.rowsFlushed += n

	return n, numRowGroups, nil
}

func (s *profileStore) ingest(_ context.Context, profiles []*schemav1.Profile, lbs phlaremodel.Labels, profileName string, rewriter *rewriter) error {
	// rewrite elements
	for pos := range profiles {
		if err := s.helper.rewrite(rewriter, profiles[pos]); err != nil {
			return err
		}
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	// do a binary search where to insert each profile
	for pos, p := range profiles {
		// check if row group is full
		if s.cfg.MaxBufferRowCount > 0 && len(s.slice) >= s.cfg.MaxBufferRowCount ||
			s.cfg.MaxRowGroupBytes > 0 && s.size.Load() >= s.cfg.MaxRowGroupBytes {
			if err := s.cutRowGroup(); err != nil {
				return err
			}
		}

		// add profile to the index
		s.index.Add(p, lbs, profileName)

		// increase size of stored data
		s.size.Add(s.helper.size(profiles[pos]))

		// add to slice
		s.slice = append(s.slice, p)

	}

	return nil
}

func (s *profileStore) NumRows() int64 {
	return int64(len(s.slice)) + int64(s.rowsFlushed)
}

type rowGroupOnDisk struct {
	parquet.RowGroup
	file          *os.File
	seriesIndexes rowRangesWithSeriesIndex
}

func newRowGroupOnDisk(path string) (*rowGroupOnDisk, error) {
	var (
		r   = &rowGroupOnDisk{}
		err error
	)

	// now open the row group file, so we are able to read the row group back in
	r.file, err = os.Open(path)
	if err != nil {
		return nil, errors.Wrapf(err, "opening row groups segment file %s", path)
	}

	stats, err := r.file.Stat()
	if err != nil {
		return nil, errors.Wrapf(err, "getting stat of row groups segment file %s", path)
	}

	segmentParquet, err := parquet.OpenFile(r.file, stats.Size())
	if err != nil {
		return nil, errors.Wrapf(err, "reading parquet of row groups segment file %s", path)
	}

	rowGroups := segmentParquet.RowGroups()
	if len(rowGroups) != 1 {
		return nil, errors.Wrapf(err, "segement file expected to have exactly one row group (actual %d)", len(rowGroups))
	}

	r.RowGroup = rowGroups[0]

	return r, nil

}

func (r *rowGroupOnDisk) RowGroups() []parquet.RowGroup {
	return []parquet.RowGroup{r.RowGroup}
}

func (r *rowGroupOnDisk) Rows() parquet.Rows {
	rows := r.RowGroup.Rows()
	if len(r.seriesIndexes) == 0 {
		return rows
	}

	return &seriesIDRowsRewriter{
		Rows:          rows,
		seriesIndexes: r.seriesIndexes,
	}
}

func (r *rowGroupOnDisk) Close() error {
	if err := r.file.Close(); err != nil {
		return err
	}

	if err := os.Remove(r.file.Name()); err != nil {
		return errors.Wrap(err, "deleting row group segment file")
	}

	return nil
}

func (r *rowGroupOnDisk) columnIter(ctx context.Context, columnName string, predicate query.Predicate, alias string) query.Iterator {
	column, found := r.RowGroup.Schema().Lookup(columnName)
	if !found {
		return query.NewErrIterator(fmt.Errorf("column '%s' not found in head row group segment '%s'", columnName, r.file.Name()))
	}
	return query.NewColumnIterator(ctx, []parquet.RowGroup{r.RowGroup}, column.ColumnIndex, columnName, 1000, predicate, alias)

}

type seriesIDRowsRewriter struct {
	parquet.Rows
	pos           int64
	seriesIndexes rowRangesWithSeriesIndex
}

func (r seriesIDRowsRewriter) SeekToRow(pos int64) error {
	if err := r.Rows.SeekToRow(pos); err != nil {
		return err
	}
	r.pos += pos
	return nil
}

var colIdxSeriesIndex = func() int {
	p := &schemav1.ProfilePersister{}
	colIdx, found := p.Schema().Lookup("SeriesIndex")
	if !found {
		panic("column SeriesIndex not found")
	}
	return colIdx.ColumnIndex
}()

func (r *seriesIDRowsRewriter) ReadRows(rows []parquet.Row) (int, error) {
	n, err := r.Rows.ReadRows(rows)
	if err != nil {
		return n, err
	}

	for pos, row := range rows[:n] {
		// actual row num
		rowNum := r.pos + int64(pos)
		row[colIdxSeriesIndex] = parquet.ValueOf(r.seriesIndexes.getSeriesIndex(rowNum)).Level(0, 0, colIdxSeriesIndex)
	}

	r.pos += int64(n)

	return n, nil
}

func mergeOnDiskByStacktraces(ctx context.Context, rows iter.Iterator[Profile], m mapAdder) error {
	var (
		currentRG *profileRowGroup
		profiles  []Profile
	)

	merge := func() error {
		if currentRG == nil {
			return nil
		}

		if err := mergeByStacktraces(ctx, currentRG.rowGroup, iter.NewSliceIterator(profiles), m); err != nil {
			return err
		}

		profiles = profiles[:0]
		currentRG = nil
		return nil
	}

	for rows.Next() {
		p, ok := rows.At().(profileOnDisk)
		// if not a on disk profile, skip
		if !ok {
			continue
		}

		// check if we are talking about the same row group as before, if not run the merge
		if currentRG != nil && currentRG != p.rowGroup {
			if err := merge(); err != nil {
				return err
			}
		}
		currentRG = p.rowGroup
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(profiles) > 0 {
		return merge()
	}

	return nil
}

func mergeOnDiskByLabels(ctx context.Context, rows iter.Iterator[Profile], m seriesByLabels, by ...string) error {
	var (
		currentRG *profileRowGroup
		profiles  []Profile
	)

	merge := func() error {
		if currentRG == nil {
			return nil
		}
		if err := mergeByLabels(ctx, currentRG.rowGroup, iter.NewSliceIterator(profiles), m, by...); err != nil {
			return err
		}

		profiles = profiles[:0]
		currentRG = nil
		return nil
	}

	for rows.Next() {
		p, ok := rows.At().(profileOnDisk)
		// if not a on disk profile, skip
		if !ok {
			continue
		}

		// check if we are talking about the same row group as before, if not run the merge
		if currentRG != nil && currentRG != p.rowGroup {
			if err := merge(); err != nil {
				return err
			}
		}
		currentRG = p.rowGroup
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(profiles) > 0 {
		return merge()
	}

	return nil
}
