package metricsdata

import (
	"fmt"
	"math"
	"sort"

	"github.com/lindb/roaring"

	"github.com/lindb/lindb/aggregation"
	"github.com/lindb/lindb/flow"
	"github.com/lindb/lindb/pkg/encoding"
	"github.com/lindb/lindb/pkg/stream"
	"github.com/lindb/lindb/series/field"
)

//go:generate mockgen -source ./reader.go -destination=./reader_mock.go -package metricsdata

const (
	dataFooterSize = 2 + // start time slot
		2 + // end time slot
		4 + // field metas position
		4 + // series ids position
		4 + // high offsets position
		4 // crc32 checksum
)

// Reader represents the metric block reader
type Reader interface {
	// GetSeriesIDs returns the series ids in this sst file
	GetSeriesIDs() *roaring.Bitmap
	// GetFields returns the field metas in this sst file
	GetFields() field.Metas
	// Load loads the data from sst file, then aggregates the data
	Load(flow flow.StorageQueryFlow, familyTime int64, fieldIDs []field.ID, highKey uint16, groupedSeries map[string][]uint16)
}

// fieldAggregator represents the field aggregator that does file data scan and aggregates
type fieldAggregator struct {
	fieldMeta  field.Meta
	aggregator aggregation.PrimitiveAggregator

	fieldKey field.Key
}

// newFieldAggregator creates a field aggregator
func newFieldAggregator(fieldMeta field.Meta, aggregator aggregation.PrimitiveAggregator) *fieldAggregator {
	fieldKey := field.Key(stream.ReadUint16([]byte{byte(fieldMeta.ID), byte(aggregator.FieldID())}, 0))
	return &fieldAggregator{
		fieldMeta:  fieldMeta,
		aggregator: aggregator,
		fieldKey:   fieldKey,
	}
}

// reader implements Reader interface that reads metric block
type reader struct {
	buf           []byte
	highOffsets   *encoding.FixedOffsetDecoder
	seriesIDs     *roaring.Bitmap
	fields        field.Metas
	crc32CheckSum uint32
	start, end    uint16
}

// NewReader creates a metric block reader
func NewReader(buf []byte) (Reader, error) {
	r := &reader{
		buf: buf,
	}
	if err := r.initReader(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetSeriesIDs returns the series ids in this sst file
func (r *reader) GetSeriesIDs() *roaring.Bitmap {
	return r.seriesIDs
}

// GetFields returns the field metas in this sst file
func (r *reader) GetFields() field.Metas {
	return r.fields
}

// prepare prepares the field aggregator based on query condition
func (r *reader) prepare(familyTime int64, fieldIDs []field.ID, aggregator aggregation.FieldAggregates) (aggs []*fieldAggregator) {
	for idx, fieldID := range fieldIDs { // sort by field ids
		fMeta, ok := r.fields.GetFromID(fieldID)
		if !ok {
			continue
		}
		fieldAggregator, ok := aggregator[idx].GetAggregator(familyTime)
		if !ok {
			continue
		}
		pAggregators := fieldAggregator.GetAllAggregators() // sort by primitive field ids
		for _, agg := range pAggregators {
			aggs = append(aggs, newFieldAggregator(fMeta, agg))
		}
	}
	return
}

// Load loads the data from sst file, then aggregates the data
func (r *reader) Load(flow flow.StorageQueryFlow, familyTime int64, fieldIDs []field.ID, highKey uint16, groupedSeries map[string][]uint16) {
	// 1. get high container index by the high key of series ID
	highContainerIdx := r.seriesIDs.GetContainerIndex(highKey)
	if highContainerIdx < 0 {
		// if high container index < 0(series IDs not exist) return it
		return
	}
	// 2. get low container include all low keys by the high container index, delete op will clean empty low container
	lowContainer := r.seriesIDs.GetContainerAtIndex(highContainerIdx)
	seriesOffsets := encoding.NewFixedOffsetDecoder(r.buf[r.highOffsets.Get(highContainerIdx):])

	//var aggregators aggregation.FieldAggregates
	tsd := encoding.GetTSDDecoder()
	defer encoding.ReleaseTSDDecoder(tsd)

	for groupByTags, lowSeriesIDs := range groupedSeries {
		aggregator := flow.GetAggregator()
		fieldAggs := r.prepare(familyTime, fieldIDs, aggregator)
		if len(fieldAggs) == 0 {
			// reduce empty aggregator for re-use
			flow.Reduce(groupByTags, aggregator)
			continue
		}

		for _, lowSeriesID := range lowSeriesIDs {
			// check low series id if exist
			if !lowContainer.Contains(lowSeriesID) {
				continue
			}
			// get the index of low series id in container
			idx := lowContainer.Rank(lowSeriesID)
			// scan the data and aggregate the values
			seriesPos := seriesOffsets.Get(idx - 1)
			// read series data and agg it
			r.readSeriesData(seriesPos, tsd, fieldAggs)
		}
		flow.Reduce(groupByTags, aggregator)
	}
}

// readSeriesData reads series data with position
func (r *reader) readSeriesData(position int, tsd *encoding.TSDDecoder, fieldAggs []*fieldAggregator) {
	fieldLength := int(stream.ReadUint16(r.buf, position))
	fieldOffsets := encoding.NewFixedOffsetDecoder(r.buf[position+2:])
	// find small/equals family id index
	idx := sort.Search(fieldLength, func(i int) bool {
		return field.Key(stream.ReadUint16(r.buf, fieldOffsets.Get(i))) >= fieldAggs[0].fieldKey
	})
	fieldCount := len(fieldAggs)
	j := 0
	for i := idx; i < fieldLength; i++ {
		agg := fieldAggs[j]
		offset := fieldOffsets.Get(i)
		key := field.Key(stream.ReadUint16(r.buf, offset))
		switch {
		case key == agg.fieldKey:
			tsd.ResetWithTimeRange(r.buf[offset+2:], r.start, r.end)
			// read field data
			r.readField(agg.aggregator, tsd)
			j++ // goto next query field id
			// found all query fields return it
			if fieldCount == j {
				return
			}
		case key > agg.fieldKey:
			// store key > query key, return it
			return
		}
	}
}

// readField reads field data and aggregates it
func (r *reader) readField(agg aggregation.PrimitiveAggregator, tsd *encoding.TSDDecoder) {
	for tsd.Next() {
		if tsd.HasValue() {
			timeSlot := tsd.Slot()
			val := tsd.Value()
			agg.Aggregate(int(timeSlot), math.Float64frombits(val))
		}
	}
}

// initReader initializes the reader context includes tag value ids/high offsets
func (r *reader) initReader() error {
	if len(r.buf) <= dataFooterSize {
		return fmt.Errorf("block length no ok")
	}
	// read footer(2+2+4+4+4+4)
	footerPos := len(r.buf) - dataFooterSize
	r.start = stream.ReadUint16(r.buf, footerPos)
	r.end = stream.ReadUint16(r.buf, footerPos+2)

	fieldMetaStartPos := int(stream.ReadUint32(r.buf, footerPos+4))
	seriesIDsStartPos := int(stream.ReadUint32(r.buf, footerPos+8))
	highOffsetsPos := int(stream.ReadUint32(r.buf, footerPos+12))
	r.crc32CheckSum = stream.ReadUint32(r.buf, footerPos+16)
	// validate offsets
	if fieldMetaStartPos > footerPos || seriesIDsStartPos > highOffsetsPos {
		return fmt.Errorf("bad offsets")
	}

	// read field metas
	offset := fieldMetaStartPos
	fieldCount := r.buf[offset]
	offset++
	r.fields = make(field.Metas, fieldCount)
	for i := byte(0); i < fieldCount; i++ {
		r.fields[i] = field.Meta{
			ID:   field.ID(r.buf[offset]),
			Type: field.Type(r.buf[offset+1]),
		}
		offset += 2
	}

	// read series ids
	seriesIDs := roaring.New()
	if err := encoding.BitmapUnmarshal(seriesIDs, r.buf[seriesIDsStartPos:]); err != nil {
		return err
	}
	r.seriesIDs = seriesIDs
	// read high offsets
	r.highOffsets = encoding.NewFixedOffsetDecoder(r.buf[highOffsetsPos:])
	return nil
}
