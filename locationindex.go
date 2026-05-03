package locationindex

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	locationid "github.com/ruckstackapp/locationid/go"
)

const persistedVersion = 1
const defaultSpatialCellPrecision = 14
const defaultHotSpatialCellThreshold = 4096
const refinedSpatialCellExtraPrecision = 2

var persistedMagic = [4]byte{'L', 'I', 'D', 'X'}
var spatialCellPrecision = uint(defaultSpatialCellPrecision)
var hotSpatialCellThreshold = defaultHotSpatialCellThreshold

var (
	ErrMissingID          = errors.New("missing record id")
	ErrMissingCode        = errors.New("missing location code")
	ErrDuplicateID        = errors.New("duplicate record id")
	ErrNotFound           = errors.New("record not found")
	ErrUnsupportedVersion = errors.New("unsupported index file version")
)

type RecordID string
type Label string
type docID uint32

type IndexedRecord struct {
	ID       RecordID          `json:"id"`
	Code     string            `json:"code"`
	Payload  string            `json:"payload"`
	Labels   []Label           `json:"labels,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type Result struct {
	Record         IndexedRecord `json:"record"`
	DistanceMeters float64       `json:"distance_meters"`
}

type SearchStats struct {
	CandidateCount      int  `json:"candidate_count,omitempty"`
	CandidateCountExact bool `json:"candidate_count_exact,omitempty"`
	ResultCount         int  `json:"result_count"`
	HasMore             bool `json:"has_more,omitempty"`
	PrefixCount         int  `json:"prefix_count,omitempty"`
	ExpansionCount      int  `json:"expansion_count,omitempty"`
	FinalRadius         int  `json:"final_radius,omitempty"`
}

type PrefixSearchResponse struct {
	Stats   SearchStats     `json:"stats"`
	Records []IndexedRecord `json:"records"`
}

type RecordSearchResponse struct {
	Stats   SearchStats     `json:"stats"`
	Records []IndexedRecord `json:"records"`
}

type ResultSearchResponse struct {
	Stats   SearchStats `json:"stats"`
	Results []Result    `json:"results"`
}

type QueryOptions struct {
	Labels              []Label
	Limit               int
	ExactCandidateCount bool
}

type BoundingBox struct {
	MinLat float64
	MaxLat float64
	MinLon float64
	MaxLon float64
}

type RadiusQuery struct {
	Lat          float64
	Lon          float64
	RadiusMeters float64
	Precision    uint
}

type PersistedIndex struct {
	Version uint            `json:"version"`
	Records []IndexedRecord `json:"records"`
}

type Set[T comparable] map[T]struct{}

func NewSet[T comparable]() Set[T] {
	return make(Set[T])
}

func (s Set[T]) Add(value T) {
	s[value] = struct{}{}
}

func (s Set[T]) Remove(value T) {
	delete(s, value)
}

func (s Set[T]) AddAll(values Set[T]) {
	for value := range values {
		s[value] = struct{}{}
	}
}

func (s Set[T]) Clone() Set[T] {
	cloned := NewSet[T]()
	for value := range s {
		cloned[value] = struct{}{}
	}
	return cloned
}

func (s Set[T]) Intersection(other Set[T]) Set[T] {
	if len(s) == 0 || len(other) == 0 {
		return NewSet[T]()
	}

	if len(other) < len(s) {
		s, other = other, s
	}

	result := NewSet[T]()
	for value := range s {
		if _, ok := other[value]; ok {
			result[value] = struct{}{}
		}
	}

	return result
}

type LocationIndex struct {
	Records              map[RecordID]IndexedRecord
	ByPayload            map[string]Set[docID]
	ByPayloadPrefix      map[string]Set[docID]
	ByLabel              map[Label]Set[docID]
	bySpatialPrefix      map[string]Set[docID]
	bySpatialCell        map[string]Set[docID]
	byRefinedSpatialCell map[string]Set[docID]
	byHotSpatialFallback map[string]Set[docID]
	hotSpatialCells      map[string]struct{}
	decodedRecords       map[docID]locationid.DecodedLocation
	docIDsByRecord       map[RecordID]docID
	recordsByDocID       map[docID]RecordID
	nextDocID            docID
}

func NewLocationIndex() *LocationIndex {
	return &LocationIndex{
		Records:              map[RecordID]IndexedRecord{},
		ByPayload:            map[string]Set[docID]{},
		ByPayloadPrefix:      map[string]Set[docID]{},
		ByLabel:              map[Label]Set[docID]{},
		bySpatialPrefix:      map[string]Set[docID]{},
		bySpatialCell:        map[string]Set[docID]{},
		byRefinedSpatialCell: map[string]Set[docID]{},
		byHotSpatialFallback: map[string]Set[docID]{},
		hotSpatialCells:      map[string]struct{}{},
		decodedRecords:       map[docID]locationid.DecodedLocation{},
		docIDsByRecord:       map[RecordID]docID{},
		recordsByDocID:       map[docID]RecordID{},
	}
}

func SetSpatialCellPrecision(precision uint) error {
	if precision == 0 || precision > 20 {
		return fmt.Errorf("spatial cell precision must be within [1, 20]")
	}

	spatialCellPrecision = precision
	return nil
}

func SpatialCellPrecision() uint {
	return spatialCellPrecision
}

func SetHotSpatialCellThreshold(threshold int) error {
	if threshold <= 0 {
		return fmt.Errorf("hot spatial cell threshold must be positive")
	}

	hotSpatialCellThreshold = threshold
	return nil
}

func HotSpatialCellThreshold() int {
	return hotSpatialCellThreshold
}

func Load(path string) (*LocationIndex, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	records, err := loadRecords(file)
	if err != nil {
		return nil, err
	}

	idx := NewLocationIndex()
	for _, record := range records {
		if err := idx.Insert(record); err != nil {
			return nil, err
		}
	}

	return idx, nil
}

func prefixes(payload string) []string {
	if payload == "" {
		return nil
	}

	out := make([]string, 0, len(payload))
	for i := 1; i <= len(payload); i++ {
		out = append(out, payload[:i])
	}

	return out
}

func (idx *LocationIndex) Insert(record IndexedRecord) error {
	if record.ID == "" {
		return ErrMissingID
	}

	if record.Code == "" {
		return ErrMissingCode
	}

	decoded, err := locationid.Decode(locationid.New(record.Code))
	if err != nil {
		return err
	}

	record.Code = strings.ToUpper(record.Code)
	record.Payload = decoded.Payload

	if _, exists := idx.Records[record.ID]; exists {
		return ErrDuplicateID
	}
	docID := idx.allocateDocID(record.ID)

	idx.Records[record.ID] = record
	idx.decodedRecords[docID] = decoded
	idx.ensurePayloadSet(record.Payload).Add(docID)
	for _, prefix := range spatialPrefixes(decoded) {
		idx.ensureSpatialPrefixSet(prefix).Add(docID)
	}
	for _, cell := range spatialCellsForBoundsAtPrecision(decoded.Bounds, spatialCellPrecision) {
		set := idx.ensureSpatialCellSet(cell)
		set.Add(docID)
		if idx.isHotSpatialCell(cell) {
			idx.indexHotSpatialRecord(cell, docID, decoded)
			continue
		}
		if len(set) > hotSpatialCellThreshold {
			idx.promoteHotSpatialCell(cell)
		}
	}

	for _, prefix := range prefixes(record.Payload) {
		idx.ensurePrefixSet(prefix).Add(docID)
	}

	for _, label := range record.Labels {
		idx.ensureLabelSet(label).Add(docID)
	}

	return nil
}

func (idx *LocationIndex) Remove(id RecordID) error {
	record, exists := idx.Records[id]
	if !exists {
		return ErrNotFound
	}
	docID, ok := idx.docIDsByRecord[id]
	if !ok {
		return ErrNotFound
	}
	decoded, hasDecoded := idx.decodedRecords[docID]

	delete(idx.Records, id)
	delete(idx.decodedRecords, docID)
	delete(idx.docIDsByRecord, id)
	delete(idx.recordsByDocID, docID)
	idx.removeFromStringKeyedSet(idx.ByPayload, record.Payload, docID)

	if hasDecoded {
		for _, prefix := range spatialPrefixes(decoded) {
			idx.removeFromStringKeyedSet(idx.bySpatialPrefix, prefix, docID)
		}
		for _, cell := range spatialCellsForBoundsAtPrecision(decoded.Bounds, spatialCellPrecision) {
			idx.removeFromStringKeyedSet(idx.bySpatialCell, cell, docID)
			if idx.isHotSpatialCell(cell) {
				idx.removeHotSpatialRecord(cell, docID, decoded)
			}
		}
	}

	for _, prefix := range prefixes(record.Payload) {
		idx.removeFromStringKeyedSet(idx.ByPayloadPrefix, prefix, docID)
	}

	for _, label := range record.Labels {
		idx.removeFromLabelSet(label, docID)
	}

	return nil
}

func (idx *LocationIndex) Update(record IndexedRecord) error {
	if _, exists := idx.Records[record.ID]; exists {
		if err := idx.Remove(record.ID); err != nil {
			return err
		}
	}

	return idx.Insert(record)
}

func (idx *LocationIndex) GetByID(id RecordID) (IndexedRecord, bool) {
	record, ok := idx.Records[id]
	return record, ok
}

func (idx *LocationIndex) SearchByPrefix(prefix string, opts QueryOptions) []IndexedRecord {
	return idx.SearchByPrefixDetailed(prefix, opts).Records
}

func (idx *LocationIndex) SearchByPrefixDetailed(prefix string, opts QueryOptions) PrefixSearchResponse {
	prefix = strings.ToUpper(prefix)
	ids := idx.ByPayloadPrefix[prefix]
	records, candidateCount, candidateCountExact, hasMore := idx.prefixRecordsForIDs(ids, opts)
	return PrefixSearchResponse{
		Stats: SearchStats{
			CandidateCount:      candidateCount,
			CandidateCountExact: candidateCountExact,
			ResultCount:         len(records),
			HasMore:             hasMore,
			PrefixCount:         boolToCount(prefix != ""),
		},
		Records: records,
	}
}

func (idx *LocationIndex) SearchBoundingBox(box BoundingBox, precision uint, opts QueryOptions) []IndexedRecord {
	return idx.SearchBoundingBoxDetailed(box, precision, opts).Records
}

func (idx *LocationIndex) SearchBoundingBoxDetailed(box BoundingBox, precision uint, opts QueryOptions) RecordSearchResponse {
	_ = precision
	candidateIDs, cellCount := idx.candidateIDsForQueryBounds(boundsFromBoundingBox(box))
	candidateIDs = idx.filterByLabels(candidateIDs, opts.Labels)

	results := make([]IndexedRecord, 0, len(candidateIDs))
	for _, docID := range sortedDocIDs(candidateIDs) {
		recordID, ok := idx.recordsByDocID[docID]
		if !ok {
			continue
		}
		record := idx.Records[recordID]
		decoded, ok := idx.decodedRecords[docID]
		if !ok {
			continue
		}

		if intersects(box, decoded.Bounds) {
			results = append(results, record)
		}
	}

	results = limitRecords(results, opts.Limit)

	return RecordSearchResponse{
		Stats: SearchStats{
			CandidateCount: len(candidateIDs),
			ResultCount:    len(results),
			PrefixCount:    cellCount,
		},
		Records: results,
	}
}

func (idx *LocationIndex) SearchRadius(q RadiusQuery, opts QueryOptions) []Result {
	return idx.SearchRadiusDetailed(q, opts).Results
}

func (idx *LocationIndex) SearchRadiusDetailed(q RadiusQuery, opts QueryOptions) ResultSearchResponse {
	_ = q.Precision
	candidateIDs, cellCount := idx.candidateIDsForQueryBounds(boundsFromBoundingBox(boundingBoxForRadius(q.Lat, q.Lon, q.RadiusMeters)))
	candidateIDs = idx.filterByLabels(candidateIDs, opts.Labels)

	results := make([]Result, 0, len(candidateIDs))
	for _, docID := range sortedDocIDs(candidateIDs) {
		recordID, ok := idx.recordsByDocID[docID]
		if !ok {
			continue
		}
		record := idx.Records[recordID]
		decoded, ok := idx.decodedRecords[docID]
		if !ok {
			continue
		}

		distance := Haversine(q.Lat, q.Lon, decoded.CenterLat, decoded.CenterLon)
		if distance <= q.RadiusMeters {
			results = append(results, Result{Record: record, DistanceMeters: distance})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].DistanceMeters == results[j].DistanceMeters {
			return results[i].Record.ID < results[j].Record.ID
		}
		return results[i].DistanceMeters < results[j].DistanceMeters
	})

	results = limitResults(results, opts.Limit)

	return ResultSearchResponse{
		Stats: SearchStats{
			CandidateCount: len(candidateIDs),
			ResultCount:    len(results),
			PrefixCount:    cellCount,
		},
		Results: results,
	}
}

func (idx *LocationIndex) SearchNearest(lat, lon float64, limit int, opts QueryOptions) []Result {
	return idx.SearchNearestDetailed(lat, lon, limit, opts).Results
}

func (idx *LocationIndex) SearchNearestDetailed(lat, lon float64, limit int, opts QueryOptions) ResultSearchResponse {
	if limit <= 0 {
		return ResultSearchResponse{}
	}

	radius := 500.0
	expansions := 0
	for {
		expansions++
		response := idx.SearchRadiusDetailed(RadiusQuery{
			Lat:          lat,
			Lon:          lon,
			RadiusMeters: radius,
			Precision:    ChoosePrecision(radius),
		}, QueryOptions{Labels: opts.Labels})

		if len(response.Results) >= limit || radius > 500_000 {
			if len(response.Results) > limit {
				response.Results = response.Results[:limit]
				response.Stats.ResultCount = len(response.Results)
			}
			response.Stats.ExpansionCount = expansions
			response.Stats.FinalRadius = int(radius)
			return response
		}

		radius *= 2
	}
}

func (idx *LocationIndex) Save(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	records := make([]IndexedRecord, 0, len(idx.Records))
	for _, id := range sortedRecordIDsFromRecords(idx.Records) {
		records = append(records, idx.Records[id])
	}

	if err := saveRecords(file, records); err != nil {
		return err
	}

	return file.Sync()
}

func (idx *LocationIndex) filterByLabels(ids Set[docID], labels []Label) Set[docID] {
	if len(labels) == 0 {
		return ids.Clone()
	}

	filtered := ids.Clone()
	for _, label := range labels {
		filtered = filtered.Intersection(idx.ByLabel[label])
	}

	return filtered
}

func (idx *LocationIndex) ensurePayloadSet(payload string) Set[docID] {
	if idx.ByPayload[payload] == nil {
		idx.ByPayload[payload] = NewSet[docID]()
	}
	return idx.ByPayload[payload]
}

func (idx *LocationIndex) ensurePrefixSet(prefix string) Set[docID] {
	if idx.ByPayloadPrefix[prefix] == nil {
		idx.ByPayloadPrefix[prefix] = NewSet[docID]()
	}
	return idx.ByPayloadPrefix[prefix]
}

func (idx *LocationIndex) ensureLabelSet(label Label) Set[docID] {
	if idx.ByLabel[label] == nil {
		idx.ByLabel[label] = NewSet[docID]()
	}
	return idx.ByLabel[label]
}

func (idx *LocationIndex) ensureSpatialPrefixSet(prefix string) Set[docID] {
	if idx.bySpatialPrefix[prefix] == nil {
		idx.bySpatialPrefix[prefix] = NewSet[docID]()
	}
	return idx.bySpatialPrefix[prefix]
}

func (idx *LocationIndex) ensureSpatialCellSet(cell string) Set[docID] {
	if idx.bySpatialCell[cell] == nil {
		idx.bySpatialCell[cell] = NewSet[docID]()
	}
	return idx.bySpatialCell[cell]
}

func (idx *LocationIndex) ensureRefinedSpatialCellSet(cell string) Set[docID] {
	if idx.byRefinedSpatialCell[cell] == nil {
		idx.byRefinedSpatialCell[cell] = NewSet[docID]()
	}
	return idx.byRefinedSpatialCell[cell]
}

func (idx *LocationIndex) ensureHotSpatialFallbackSet(cell string) Set[docID] {
	if idx.byHotSpatialFallback[cell] == nil {
		idx.byHotSpatialFallback[cell] = NewSet[docID]()
	}
	return idx.byHotSpatialFallback[cell]
}

func (idx *LocationIndex) removeFromStringKeyedSet(sets map[string]Set[docID], key string, id docID) {
	set := sets[key]
	if set == nil {
		return
	}

	set.Remove(id)
	if len(set) == 0 {
		delete(sets, key)
	}
}

func (idx *LocationIndex) removeFromLabelSet(label Label, id docID) {
	set := idx.ByLabel[label]
	if set == nil {
		return
	}

	set.Remove(id)
	if len(set) == 0 {
		delete(idx.ByLabel, label)
	}
}

func (idx *LocationIndex) recordsForIDs(ids Set[docID], limit int) []IndexedRecord {
	records := make([]IndexedRecord, 0, len(ids))
	for _, docID := range sortedDocIDs(ids) {
		recordID, ok := idx.recordsByDocID[docID]
		if !ok {
			continue
		}
		records = append(records, idx.Records[recordID])
	}
	return limitRecords(records, limit)
}

func (idx *LocationIndex) prefixRecordsForIDs(ids Set[docID], opts QueryOptions) ([]IndexedRecord, int, bool, bool) {
	if opts.Limit <= 0 || opts.ExactCandidateCount {
		filtered := idx.filterByLabels(ids, opts.Labels)
		return idx.recordsForIDs(filtered, opts.Limit), len(filtered), true, false
	}

	records := make([]IndexedRecord, 0, opts.Limit)
	hasMore := false
	for docID := range ids {
		recordID, ok := idx.recordsByDocID[docID]
		if !ok {
			continue
		}
		record, ok := idx.Records[recordID]
		if !ok || !recordHasAllLabels(record, opts.Labels) {
			continue
		}

		if len(records) < opts.Limit {
			records = append(records, record)
			continue
		}

		hasMore = true
		break
	}

	return records, 0, false, hasMore
}

func (idx *LocationIndex) candidateIDsForPrefixes(prefixes []string) Set[docID] {
	if len(prefixes) == 0 {
		return NewSet[docID]()
	}

	ids := NewSet[docID]()
	for _, prefix := range prefixes {
		ids.AddAll(idx.ByPayloadPrefix[prefix])
	}
	return ids
}

func (idx *LocationIndex) candidateIDsForSpatialPrefixes(prefixes []string) Set[docID] {
	if len(prefixes) == 0 {
		return NewSet[docID]()
	}

	ids := NewSet[docID]()
	for _, prefix := range prefixes {
		ids.AddAll(idx.bySpatialPrefix[prefix])
	}
	return ids
}

func (idx *LocationIndex) candidateIDsForSpatialCells(cells []string) Set[docID] {
	if len(cells) == 0 {
		return NewSet[docID]()
	}

	ids := NewSet[docID]()
	for _, cell := range cells {
		ids.AddAll(idx.bySpatialCell[cell])
	}
	return ids
}

func (idx *LocationIndex) candidateIDsForQueryBounds(bounds locationid.Bounds) (Set[docID], int) {
	baseCells := spatialCellsForBoundsAtPrecision(bounds, spatialCellPrecision)
	usedCells := 0
	needsRefined := false
	for _, cell := range baseCells {
		if idx.isHotSpatialCell(cell) {
			needsRefined = true
			break
		}
	}

	ids := NewSet[docID]()
	if !needsRefined {
		for _, cell := range baseCells {
			usedCells++
			ids.AddAll(idx.bySpatialCell[cell])
		}
		return ids, usedCells
	}

	refinedPrecision := idx.refinedSpatialCellPrecision()
	refinedCells := spatialCellsForBoundsAtPrecision(bounds, refinedPrecision)
	for _, cell := range refinedCells {
		parent := parentSpatialCell(cell, spatialCellPrecision)
		if !idx.isHotSpatialCell(parent) {
			continue
		}
		usedCells++
		ids.AddAll(idx.byRefinedSpatialCell[cell])
	}

	for _, cell := range baseCells {
		if !idx.isHotSpatialCell(cell) {
			continue
		}
		usedCells++
		ids.AddAll(idx.byHotSpatialFallback[cell])
	}

	for _, cell := range baseCells {
		if idx.isHotSpatialCell(cell) {
			continue
		}
		usedCells++
		ids.AddAll(idx.bySpatialCell[cell])
	}

	return ids, usedCells
}

func limitRecords(records []IndexedRecord, limit int) []IndexedRecord {
	if limit > 0 && len(records) > limit {
		return records[:limit]
	}
	return records
}

func limitResults(results []Result, limit int) []Result {
	if limit > 0 && len(results) > limit {
		return results[:limit]
	}
	return results
}

func recordHasAllLabels(record IndexedRecord, labels []Label) bool {
	if len(labels) == 0 {
		return true
	}

	if len(record.Labels) < len(labels) {
		return false
	}

	available := make(map[Label]struct{}, len(record.Labels))
	for _, label := range record.Labels {
		available[label] = struct{}{}
	}

	for _, label := range labels {
		if _, ok := available[label]; !ok {
			return false
		}
	}

	return true
}

func sortedDocIDs(ids Set[docID]) []docID {
	ordered := make([]docID, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i] < ordered[j]
	})
	return ordered
}

func sortedRecordIDsFromRecords(records map[RecordID]IndexedRecord) []RecordID {
	ordered := make([]RecordID, 0, len(records))
	for id := range records {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i] < ordered[j]
	})
	return ordered
}

func (idx *LocationIndex) allocateDocID(recordID RecordID) docID {
	idx.nextDocID++
	docID := idx.nextDocID
	idx.docIDsByRecord[recordID] = docID
	idx.recordsByDocID[docID] = recordID
	return docID
}

func (idx *LocationIndex) refinedSpatialCellPrecision() uint {
	precision := spatialCellPrecision + refinedSpatialCellExtraPrecision
	if precision > 20 {
		return 20
	}
	return precision
}

func (idx *LocationIndex) isHotSpatialCell(cell string) bool {
	_, ok := idx.hotSpatialCells[cell]
	return ok
}

func (idx *LocationIndex) promoteHotSpatialCell(cell string) {
	if idx.isHotSpatialCell(cell) {
		return
	}

	idx.hotSpatialCells[cell] = struct{}{}
	for docID := range idx.bySpatialCell[cell] {
		decoded, ok := idx.decodedRecords[docID]
		if !ok {
			continue
		}
		idx.indexHotSpatialRecord(cell, docID, decoded)
	}
}

func (idx *LocationIndex) indexHotSpatialRecord(baseCell string, id docID, decoded locationid.DecodedLocation) {
	if !idx.canRefineSpatialRecord(decoded) {
		idx.ensureHotSpatialFallbackSet(baseCell).Add(id)
		return
	}

	idx.indexRefinedSpatialCells(baseCell, id, decoded.Bounds)
}

func (idx *LocationIndex) removeHotSpatialRecord(baseCell string, id docID, decoded locationid.DecodedLocation) {
	if !idx.canRefineSpatialRecord(decoded) {
		idx.removeFromStringKeyedSet(idx.byHotSpatialFallback, baseCell, id)
		return
	}

	idx.removeRefinedSpatialCells(baseCell, id, decoded.Bounds)
}

func (idx *LocationIndex) canRefineSpatialRecord(decoded locationid.DecodedLocation) bool {
	return decoded.Precision+refinedSpatialCellExtraPrecision >= idx.refinedSpatialCellPrecision()
}

func (idx *LocationIndex) indexRefinedSpatialCells(baseCell string, id docID, bounds locationid.Bounds) {
	for _, cell := range spatialCellsForBoundsAtPrecision(bounds, idx.refinedSpatialCellPrecision()) {
		if parentSpatialCell(cell, spatialCellPrecision) != baseCell {
			continue
		}
		idx.ensureRefinedSpatialCellSet(cell).Add(id)
	}
}

func (idx *LocationIndex) removeRefinedSpatialCells(baseCell string, id docID, bounds locationid.Bounds) {
	for _, cell := range spatialCellsForBoundsAtPrecision(bounds, idx.refinedSpatialCellPrecision()) {
		if parentSpatialCell(cell, spatialCellPrecision) != baseCell {
			continue
		}
		idx.removeFromStringKeyedSet(idx.byRefinedSpatialCell, cell, id)
	}
}

func intersects(box BoundingBox, bounds locationid.Bounds) bool {
	if box.MaxLat < bounds.MinLat || box.MinLat > bounds.MaxLat {
		return false
	}

	for _, boxRange := range splitLonRange(box.MinLon, box.MaxLon) {
		if boxRange[1] >= bounds.MinLon && boxRange[0] <= bounds.MaxLon {
			return true
		}
	}

	return false
}

func coverSpatialPrefixesForBox(box BoundingBox, precision uint, stopOnContainment bool) []string {
	if precision == 0 {
		return nil
	}

	if box.MinLat > box.MaxLat {
		box.MinLat, box.MaxLat = box.MaxLat, box.MinLat
	}

	box.MinLat = clamp(box.MinLat, -90, 90)
	box.MaxLat = clamp(box.MaxLat, -90, 90)

	prefixes := make([]string, 0)
	var walk func(prefix string)
	walk = func(prefix string) {
		bounds := spatialPrefixBounds(prefix)
		if !intersects(box, bounds) {
			return
		}

		if uint(len(prefix)/2) == precision || (stopOnContainment && boxContainsBounds(box, bounds)) {
			prefixes = append(prefixes, prefix)
			return
		}

		for _, suffix := range [4]string{"00", "01", "10", "11"} {
			walk(prefix + suffix)
		}
	}

	for _, prefix := range [4]string{"00", "01", "10", "11"} {
		walk(prefix)
	}

	return prefixes
}

func spatialCellsForBounds(bounds locationid.Bounds) []string {
	return spatialCellsForBoundsAtPrecision(bounds, spatialCellPrecision)
}

func spatialCellsForBoundsAtPrecision(bounds locationid.Bounds, precision uint) []string {
	return coverSpatialPrefixesForBox(boundingBoxFromBounds(bounds), precision, false)
}

func parentSpatialCell(cell string, precision uint) string {
	length := int(precision * 2)
	if length <= 0 || len(cell) <= length {
		return cell
	}
	return cell[:length]
}

func Haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusMeters = 6371008.8

	phi1 := lat1 * math.Pi / 180
	phi2 := lat2 * math.Pi / 180
	dPhi := (lat2 - lat1) * math.Pi / 180
	dLambda := (lon2 - lon1) * math.Pi / 180

	a := math.Sin(dPhi/2)*math.Sin(dPhi/2) +
		math.Cos(phi1)*math.Cos(phi2)*math.Sin(dLambda/2)*math.Sin(dLambda/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadiusMeters * c
}

func ChoosePrecision(radiusMeters float64) uint {
	if radiusMeters <= 0 {
		return 31
	}

	for precision := uint(31); precision > 0; precision-- {
		if cellDiagonalMeters(precision) >= radiusMeters {
			return precision
		}
	}

	return 0
}

func payloadLengthForPrecision(precision uint) int {
	if precision == 0 {
		return 0
	}
	return int((precision*2 + 4) / 5)
}

func spatialPrefixes(decoded locationid.DecodedLocation) []string {
	bits := spatialBits(decoded)
	if bits == "" {
		return nil
	}

	prefixes := make([]string, 0, len(bits)/2)
	for i := 2; i <= len(bits); i += 2 {
		prefixes = append(prefixes, bits[:i])
	}
	return prefixes
}

func spatialBits(decoded locationid.DecodedLocation) string {
	encoded, ok := decodePayloadPrefix(decoded.Payload)
	if !ok {
		return ""
	}

	totalBits := len(decoded.Payload) * 5
	usefulBits := int(decoded.Precision * 2)
	padBits := totalBits - usefulBits
	if usefulBits <= 0 || padBits < 0 {
		return ""
	}

	bits := make([]byte, totalBits)
	for i := 0; i < totalBits; i++ {
		shift := totalBits - 1 - i
		if (encoded>>shift)&1 == 1 {
			bits[i] = '1'
			continue
		}
		bits[i] = '0'
	}

	return string(bits[padBits:])
}

func cellDiagonalMeters(precision uint) float64 {
	cellCount := float64(uint64(1) << precision)
	latMeters := 180.0 / cellCount * 111320.0
	lonMeters := 360.0 / cellCount * 111320.0
	return math.Hypot(latMeters, lonMeters)
}

func boundingBoxForRadius(lat, lon, radiusMeters float64) BoundingBox {
	const metersPerDegreeLat = 111320.0

	latDelta := radiusMeters / metersPerDegreeLat
	cosLat := math.Cos(lat * math.Pi / 180)
	if math.Abs(cosLat) < 1e-12 {
		cosLat = 1e-12
	}
	lonDelta := radiusMeters / (metersPerDegreeLat * math.Abs(cosLat))

	return BoundingBox{
		MinLat: clamp(lat-latDelta, -90, 90),
		MaxLat: clamp(lat+latDelta, -90, 90),
		MinLon: normalizeLon(lon - lonDelta),
		MaxLon: normalizeLon(lon + lonDelta),
	}
}

func boundingBoxFromBounds(bounds locationid.Bounds) BoundingBox {
	return BoundingBox{
		MinLat: bounds.MinLat,
		MaxLat: bounds.MaxLat,
		MinLon: bounds.MinLon,
		MaxLon: bounds.MaxLon,
	}
}

func boundsFromBoundingBox(box BoundingBox) locationid.Bounds {
	return locationid.Bounds{
		MinLat: box.MinLat,
		MaxLat: box.MaxLat,
		MinLon: box.MinLon,
		MaxLon: box.MaxLon,
	}
}

func splitLonRange(minLon, maxLon float64) [][2]float64 {
	minLon = normalizeLon(minLon)
	maxLon = normalizeLon(maxLon)
	if minLon <= maxLon {
		return [][2]float64{{minLon, maxLon}}
	}
	return [][2]float64{{-180, maxLon}, {minLon, 180}}
}

func normalizeLon(lon float64) float64 {
	for lon < -180 {
		lon += 360
	}
	for lon > 180 {
		lon -= 360
	}
	return lon
}

func boxContainsBounds(box BoundingBox, bounds locationid.Bounds) bool {
	if box.MinLat > bounds.MinLat || box.MaxLat < bounds.MaxLat {
		return false
	}

	for _, lonRange := range splitLonRange(box.MinLon, box.MaxLon) {
		if lonRange[0] <= bounds.MinLon && lonRange[1] >= bounds.MaxLon {
			return true
		}
	}

	return false
}

func decodePayloadPrefix(prefix string) (uint64, bool) {
	var value uint64
	for _, char := range strings.ToUpper(prefix) {
		index := strings.IndexRune(locationid.Alphabet, char)
		if index < 0 {
			return 0, false
		}
		value = value*32 + uint64(index)
	}
	return value, true
}

func spatialPrefixBounds(prefix string) locationid.Bounds {
	if prefix == "" {
		return locationid.Bounds{MinLat: -90, MaxLat: 90, MinLon: -180, MaxLon: 180}
	}

	var lonPrefix uint64
	var latPrefix uint64
	for i := 0; i < len(prefix); i += 2 {
		lonPrefix <<= 1
		if prefix[i] == '1' {
			lonPrefix |= 1
		}

		latPrefix <<= 1
		if prefix[i+1] == '1' {
			latPrefix |= 1
		}
	}

	precision := uint(len(prefix) / 2)
	divisor := float64(uint64(1) << precision)

	return locationid.Bounds{
		MinLat: -90.0 + 180.0*float64(latPrefix)/divisor,
		MaxLat: -90.0 + 180.0*float64(latPrefix+1)/divisor,
		MinLon: -180.0 + 360.0*float64(lonPrefix)/divisor,
		MaxLon: -180.0 + 360.0*float64(lonPrefix+1)/divisor,
	}
}

func clamp(value, minValue, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func boolToCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func saveRecords(writer io.Writer, records []IndexedRecord) error {
	buffered := bufio.NewWriter(writer)
	if _, err := buffered.Write(persistedMagic[:]); err != nil {
		return err
	}

	if err := binary.Write(buffered, binary.BigEndian, uint32(persistedVersion)); err != nil {
		return err
	}

	if err := binary.Write(buffered, binary.BigEndian, uint32(len(records))); err != nil {
		return err
	}

	for _, record := range records {
		if err := writeRecord(buffered, record); err != nil {
			return err
		}
	}

	return buffered.Flush()
}

func loadRecords(reader io.Reader) ([]IndexedRecord, error) {
	buffered := bufio.NewReader(reader)
	magic := [4]byte{}
	if _, err := io.ReadFull(buffered, magic[:]); err != nil {
		return nil, err
	}
	if magic != persistedMagic {
		return nil, ErrUnsupportedVersion
	}

	var version uint32
	if err := binary.Read(buffered, binary.BigEndian, &version); err != nil {
		return nil, err
	}
	if version != persistedVersion {
		return nil, ErrUnsupportedVersion
	}

	var count uint32
	if err := binary.Read(buffered, binary.BigEndian, &count); err != nil {
		return nil, err
	}

	records := make([]IndexedRecord, 0, count)
	for i := uint32(0); i < count; i++ {
		record, err := readRecord(buffered)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	return records, nil
}

func writeRecord(writer io.Writer, record IndexedRecord) error {
	if err := writeString(writer, string(record.ID)); err != nil {
		return err
	}
	if err := writeString(writer, record.Code); err != nil {
		return err
	}
	if err := writeString(writer, record.Payload); err != nil {
		return err
	}

	if err := binary.Write(writer, binary.BigEndian, uint32(len(record.Labels))); err != nil {
		return err
	}
	for _, label := range record.Labels {
		if err := writeString(writer, string(label)); err != nil {
			return err
		}
	}

	if err := binary.Write(writer, binary.BigEndian, uint32(len(record.Metadata))); err != nil {
		return err
	}
	for _, key := range sortedMetadataKeys(record.Metadata) {
		if err := writeString(writer, key); err != nil {
			return err
		}
		if err := writeString(writer, record.Metadata[key]); err != nil {
			return err
		}
	}

	return nil
}

func readRecord(reader io.Reader) (IndexedRecord, error) {
	id, err := readString(reader)
	if err != nil {
		return IndexedRecord{}, err
	}
	code, err := readString(reader)
	if err != nil {
		return IndexedRecord{}, err
	}
	payload, err := readString(reader)
	if err != nil {
		return IndexedRecord{}, err
	}

	var labelCount uint32
	if err := binary.Read(reader, binary.BigEndian, &labelCount); err != nil {
		return IndexedRecord{}, err
	}
	labels := make([]Label, 0, labelCount)
	for i := uint32(0); i < labelCount; i++ {
		label, err := readString(reader)
		if err != nil {
			return IndexedRecord{}, err
		}
		labels = append(labels, Label(label))
	}

	var metadataCount uint32
	if err := binary.Read(reader, binary.BigEndian, &metadataCount); err != nil {
		return IndexedRecord{}, err
	}
	metadata := make(map[string]string, metadataCount)
	for i := uint32(0); i < metadataCount; i++ {
		key, err := readString(reader)
		if err != nil {
			return IndexedRecord{}, err
		}
		value, err := readString(reader)
		if err != nil {
			return IndexedRecord{}, err
		}
		metadata[key] = value
	}

	if len(metadata) == 0 {
		metadata = nil
	}

	return IndexedRecord{
		ID:       RecordID(id),
		Code:     code,
		Payload:  payload,
		Labels:   labels,
		Metadata: metadata,
	}, nil
}

func writeString(writer io.Writer, value string) error {
	if err := binary.Write(writer, binary.BigEndian, uint32(len(value))); err != nil {
		return err
	}
	if _, err := io.WriteString(writer, value); err != nil {
		return err
	}
	return nil
}

func readString(reader io.Reader) (string, error) {
	var length uint32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return "", err
	}
	if length == 0 {
		return "", nil
	}

	buffer := make([]byte, length)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return "", err
	}
	return string(buffer), nil
}

func sortedMetadataKeys(metadata map[string]string) []string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
