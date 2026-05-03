package locationindex

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"

	locationid "github.com/ruckstackapp/locationid/go"
)

const persistedVersion = 4
const defaultSpatialCellPrecision = 14
const defaultHotSpatialCellThreshold = 4096
const refinedSpatialCellExtraPrecision = 2

var persistedMagic = [4]byte{'L', 'I', 'D', 'X'}

var (
	ErrMissingID          = errors.New("missing record id")
	ErrMissingCode        = errors.New("missing location code")
	ErrDuplicateID        = errors.New("duplicate record id")
	ErrNotFound           = errors.New("record not found")
	ErrUnsupportedVersion = errors.New("unsupported index file version")
	ErrCorruptIndex       = errors.New("corrupt index file")
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

type Snapshot struct {
	Options IndexOptions    `json:"options"`
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

type IndexStats struct {
	Options                        IndexOptions `json:"options"`
	RecordCount                    int          `json:"record_count"`
	DocIDCount                     int          `json:"doc_id_count"`
	PayloadKeyCount                int          `json:"payload_key_count"`
	PayloadPostingCount            int          `json:"payload_posting_count"`
	PayloadPrefixKeyCount          int          `json:"payload_prefix_key_count"`
	PayloadPrefixPostingCount      int          `json:"payload_prefix_posting_count"`
	LabelKeyCount                  int          `json:"label_key_count"`
	LabelPostingCount              int          `json:"label_posting_count"`
	SpatialPrefixKeyCount          int          `json:"spatial_prefix_key_count"`
	SpatialPrefixPostingCount      int          `json:"spatial_prefix_posting_count"`
	SpatialCellKeyCount            int          `json:"spatial_cell_key_count"`
	SpatialCellPostingCount        int          `json:"spatial_cell_posting_count"`
	RefinedSpatialKeyCount         int          `json:"refined_spatial_key_count"`
	RefinedSpatialPostingCount     int          `json:"refined_spatial_posting_count"`
	HotSpatialFallbackKeyCount     int          `json:"hot_spatial_fallback_key_count"`
	HotSpatialFallbackPostingCount int          `json:"hot_spatial_fallback_posting_count"`
	HotSpatialCellCount            int          `json:"hot_spatial_cell_count"`
	DecodedRecordCount             int          `json:"decoded_record_count"`
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

type IndexOptions struct {
	SpatialCellPrecision    uint `json:"spatial_cell_precision"`
	HotSpatialCellThreshold int  `json:"hot_spatial_cell_threshold"`
}

type PersistedIndex struct {
	Version uint            `json:"version"`
	Records []IndexedRecord `json:"records"`
}

type Set[T comparable] map[T]struct{}

func NewSet[T comparable]() Set[T] {
	return make(Set[T])
}

func NewSetSize[T comparable](size int) Set[T] {
	if size <= 0 {
		return make(Set[T])
	}
	return make(Set[T], size)
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
	mu                   sync.RWMutex
	Options              IndexOptions
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
	return NewLocationIndexWithOptions(DefaultIndexOptions())
}

func NewLocationIndexWithOptions(options IndexOptions) *LocationIndex {
	options = normalizeIndexOptions(options)
	return &LocationIndex{
		Options:              options,
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

func (idx *LocationIndex) ValidateOptions() error {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return validateIndexOptions(idx.Options)
}

func (idx *LocationIndex) Stats() IndexStats {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return IndexStats{
		Options:                        idx.Options,
		RecordCount:                    len(idx.Records),
		DocIDCount:                     len(idx.recordsByDocID),
		PayloadKeyCount:                len(idx.ByPayload),
		PayloadPostingCount:            countDocIDSetMapEntries(idx.ByPayload),
		PayloadPrefixKeyCount:          len(idx.ByPayloadPrefix),
		PayloadPrefixPostingCount:      countDocIDSetMapEntries(idx.ByPayloadPrefix),
		LabelKeyCount:                  len(idx.ByLabel),
		LabelPostingCount:              countLabelDocIDSetMapEntries(idx.ByLabel),
		SpatialPrefixKeyCount:          len(idx.bySpatialPrefix),
		SpatialPrefixPostingCount:      countDocIDSetMapEntries(idx.bySpatialPrefix),
		SpatialCellKeyCount:            len(idx.bySpatialCell),
		SpatialCellPostingCount:        countDocIDSetMapEntries(idx.bySpatialCell),
		RefinedSpatialKeyCount:         len(idx.byRefinedSpatialCell),
		RefinedSpatialPostingCount:     countDocIDSetMapEntries(idx.byRefinedSpatialCell),
		HotSpatialFallbackKeyCount:     len(idx.byHotSpatialFallback),
		HotSpatialFallbackPostingCount: countDocIDSetMapEntries(idx.byHotSpatialFallback),
		HotSpatialCellCount:            len(idx.hotSpatialCells),
		DecodedRecordCount:             len(idx.decodedRecords),
	}
}

func DefaultIndexOptions() IndexOptions {
	return IndexOptions{
		SpatialCellPrecision:    defaultSpatialCellPrecision,
		HotSpatialCellThreshold: defaultHotSpatialCellThreshold,
	}
}

func validateIndexOptions(options IndexOptions) error {
	if options.SpatialCellPrecision == 0 || options.SpatialCellPrecision > 20 {
		return fmt.Errorf("spatial cell precision must be within [1, 20]")
	}
	if options.HotSpatialCellThreshold <= 0 {
		return fmt.Errorf("hot spatial cell threshold must be positive")
	}
	return nil
}

func normalizeIndexOptions(options IndexOptions) IndexOptions {
	defaults := DefaultIndexOptions()
	if options.SpatialCellPrecision == 0 {
		options.SpatialCellPrecision = defaults.SpatialCellPrecision
	}
	if options.HotSpatialCellThreshold == 0 {
		options.HotSpatialCellThreshold = defaults.HotSpatialCellThreshold
	}
	return options
}

func SetSpatialCellPrecision(precision uint) error {
	if precision == 0 || precision > 20 {
		return fmt.Errorf("spatial cell precision must be within [1, 20]")
	}
	return nil
}

func SpatialCellPrecision() uint {
	return defaultSpatialCellPrecision
}

func SetHotSpatialCellThreshold(threshold int) error {
	if threshold <= 0 {
		return fmt.Errorf("hot spatial cell threshold must be positive")
	}
	return nil
}

func HotSpatialCellThreshold() int {
	return defaultHotSpatialCellThreshold
}

func Load(path string) (*LocationIndex, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	return loadIndex(file, info.Size())
}

// Open loads a persisted index snapshot from disk.
//
// Open is an alias for Load and exists to read more naturally in service
// lifecycle code.
func Open(path string) (*LocationIndex, error) {
	return Load(path)
}

// RebuildFromSnapshot constructs a fresh mutable index from a point-in-time
// snapshot.
//
// This is the preferred rebuild primitive for services that want an explicit
// snapshot-to-index workflow instead of mutating an existing live instance.
func RebuildFromSnapshot(snapshot Snapshot) (*LocationIndex, error) {
	idx := NewLocationIndexWithOptions(snapshot.Options)
	if err := idx.ValidateOptions(); err != nil {
		return nil, err
	}
	for _, record := range snapshot.Records {
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

// Insert adds a record and eagerly updates all derived postings and caches.
func (idx *LocationIndex) Insert(record IndexedRecord) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.insertLocked(record)
}

func (idx *LocationIndex) insertLocked(record IndexedRecord) error {
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
	for _, cell := range spatialCellsForBoundsAtPrecision(decoded.Bounds, idx.Options.SpatialCellPrecision) {
		set := idx.ensureSpatialCellSet(cell)
		set.Add(docID)
		if idx.isHotSpatialCell(cell) {
			idx.indexHotSpatialRecord(cell, docID, decoded)
			continue
		}
		if len(set) > idx.Options.HotSpatialCellThreshold {
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

// Remove deletes a record and eagerly updates all derived postings and caches.
func (idx *LocationIndex) Remove(id RecordID) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.removeLocked(id)
}

func (idx *LocationIndex) removeLocked(id RecordID) error {
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
		for _, cell := range spatialCellsForBoundsAtPrecision(decoded.Bounds, idx.Options.SpatialCellPrecision) {
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

// Update replaces an existing record by removing the old state and inserting the
// new state within the same mutable index instance.
//
// Services that need staged or copy-on-write style updates should prefer Clone
// or Snapshot plus RebuildFromSnapshot.
func (idx *LocationIndex) Update(record IndexedRecord) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.updateLocked(record)
}

func (idx *LocationIndex) updateLocked(record IndexedRecord) error {
	if _, exists := idx.Records[record.ID]; exists {
		if err := idx.removeLocked(record.ID); err != nil {
			return err
		}
	}

	return idx.insertLocked(record)
}

func (idx *LocationIndex) GetByID(id RecordID) (IndexedRecord, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	record, ok := idx.Records[id]
	return record, ok
}

func (idx *LocationIndex) SearchByPrefix(prefix string, opts QueryOptions) []IndexedRecord {
	return idx.SearchByPrefixDetailed(prefix, opts).Records
}

func (idx *LocationIndex) SearchByPrefixDetailed(prefix string, opts QueryOptions) PrefixSearchResponse {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
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
	idx.mu.RLock()
	defer idx.mu.RUnlock()
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
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.searchRadiusDetailedLocked(q, opts)
}

func (idx *LocationIndex) searchRadiusDetailedLocked(q RadiusQuery, opts QueryOptions) ResultSearchResponse {
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
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if limit <= 0 {
		return ResultSearchResponse{}
	}

	radius := 500.0
	expansions := 0
	for {
		expansions++
		response := idx.searchRadiusDetailedLocked(RadiusQuery{
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

// Save writes the current in-memory index as an atomic persisted snapshot.
//
// The persistence model is snapshot-based rather than incremental: each save
// writes a complete durable representation of the current index state.
func (idx *LocationIndex) Save(path string) error {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	records := make([]IndexedRecord, 0, len(idx.Records))
	for _, id := range sortedRecordIDsFromDocIDs(idx.recordsByDocID) {
		records = append(records, idx.Records[id])
	}

	return writeAtomically(path, func(file *os.File) error {
		if err := saveIndex(file, idx, records); err != nil {
			return err
		}
		return file.Sync()
	})
}

// Snapshot exports a point-in-time copy of the public index contents and index
// options.
//
// The returned snapshot is suitable for rebuild workflows and service-managed
// staged mutation flows.
func (idx *LocationIndex) Snapshot() Snapshot {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	records := make([]IndexedRecord, 0, len(idx.Records))
	for _, id := range sortedRecordIDsFromDocIDs(idx.recordsByDocID) {
		records = append(records, idx.Records[id])
	}

	return Snapshot{
		Options: idx.Options,
		Records: records,
	}
}

// Clone rebuilds a logically equivalent index from the current snapshot.
//
// Clone is useful when a caller wants to stage mutations on a separate index
// instance without sharing internal mutable state with the original.
func (idx *LocationIndex) Clone() (*LocationIndex, error) {
	return RebuildFromSnapshot(idx.Snapshot())
}

func writeAtomically(path string, write func(file *os.File) error) error {
	dir := filepathDir(path)
	tempFile, err := os.CreateTemp(dir, ".locationindex-*")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer func() {
		tempFile.Close()
		_ = os.Remove(tempPath)
	}()

	if err := write(tempFile); err != nil {
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func filepathDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
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
	baseCells := spatialCellsForBoundsAtPrecision(bounds, idx.Options.SpatialCellPrecision)
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
		parent := parentSpatialCell(cell, idx.Options.SpatialCellPrecision)
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

func sortedRecordIDsFromDocIDs(recordsByDocID map[docID]RecordID) []RecordID {
	orderedDocIDs := make([]docID, 0, len(recordsByDocID))
	for id := range recordsByDocID {
		orderedDocIDs = append(orderedDocIDs, id)
	}
	sort.Slice(orderedDocIDs, func(i, j int) bool {
		return orderedDocIDs[i] < orderedDocIDs[j]
	})

	recordIDs := make([]RecordID, 0, len(orderedDocIDs))
	for _, id := range orderedDocIDs {
		recordIDs = append(recordIDs, recordsByDocID[id])
	}
	return recordIDs
}

func (idx *LocationIndex) allocateDocID(recordID RecordID) docID {
	idx.nextDocID++
	docID := idx.nextDocID
	idx.docIDsByRecord[recordID] = docID
	idx.recordsByDocID[docID] = recordID
	return docID
}

func (idx *LocationIndex) refinedSpatialCellPrecision() uint {
	precision := idx.Options.SpatialCellPrecision + refinedSpatialCellExtraPrecision
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
		if parentSpatialCell(cell, idx.Options.SpatialCellPrecision) != baseCell {
			continue
		}
		idx.ensureRefinedSpatialCellSet(cell).Add(id)
	}
}

func (idx *LocationIndex) removeRefinedSpatialCells(baseCell string, id docID, bounds locationid.Bounds) {
	for _, cell := range spatialCellsForBoundsAtPrecision(bounds, idx.refinedSpatialCellPrecision()) {
		if parentSpatialCell(cell, idx.Options.SpatialCellPrecision) != baseCell {
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

func saveIndex(writer io.Writer, idx *LocationIndex, records []IndexedRecord) error {
	buffered := bufio.NewWriter(writer)
	if _, err := buffered.Write(persistedMagic[:]); err != nil {
		return err
	}

	if err := binary.Write(buffered, binary.BigEndian, uint32(persistedVersion)); err != nil {
		return err
	}
	bodyChecksum := crc32.NewIEEE()
	bodyWriter := io.MultiWriter(buffered, bodyChecksum)

	if err := binary.Write(bodyWriter, binary.BigEndian, uint32(idx.Options.SpatialCellPrecision)); err != nil {
		return err
	}
	if err := binary.Write(bodyWriter, binary.BigEndian, uint32(idx.Options.HotSpatialCellThreshold)); err != nil {
		return err
	}

	if err := binary.Write(bodyWriter, binary.BigEndian, uint32(len(records))); err != nil {
		return err
	}

	for _, record := range records {
		docID := idx.docIDsByRecord[record.ID]
		if err := binary.Write(bodyWriter, binary.BigEndian, uint32(docID)); err != nil {
			return err
		}
		if err := writeRecord(bodyWriter, record); err != nil {
			return err
		}
		if err := writeDecodedLocation(bodyWriter, idx.decodedRecords[docID]); err != nil {
			return err
		}
	}

	if err := writeDocIDSetMap(bodyWriter, idx.ByPayload); err != nil {
		return err
	}
	if err := writeLabelDocIDSetMap(bodyWriter, idx.ByLabel); err != nil {
		return err
	}
	if err := writeDocIDSetMap(bodyWriter, idx.ByPayloadPrefix); err != nil {
		return err
	}
	if err := writeDocIDSetMap(bodyWriter, idx.bySpatialPrefix); err != nil {
		return err
	}
	if err := writeDocIDSetMap(bodyWriter, idx.bySpatialCell); err != nil {
		return err
	}
	if err := writeDocIDSetMap(bodyWriter, idx.byRefinedSpatialCell); err != nil {
		return err
	}
	if err := writeDocIDSetMap(bodyWriter, idx.byHotSpatialFallback); err != nil {
		return err
	}
	if err := writeStringSet(bodyWriter, idx.hotSpatialCells); err != nil {
		return err
	}
	if err := binary.Write(buffered, binary.BigEndian, bodyChecksum.Sum32()); err != nil {
		return err
	}

	return buffered.Flush()
}

func loadIndex(file *os.File, size int64) (*LocationIndex, error) {
	if size < 8 {
		return nil, ErrCorruptIndex
	}

	buffered := bufio.NewReader(file)
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
	if version == 1 {
		return loadIndexV1(buffered)
	}
	if version == 2 {
		return loadIndexV2(buffered)
	}
	if version == 3 {
		return loadIndexV3(file, size)
	}
	if version != persistedVersion {
		return nil, ErrUnsupportedVersion
	}
	return loadChecksummedIndex(file, size, true)
}

func loadIndexV3(file *os.File, size int64) (*LocationIndex, error) {
	return loadChecksummedIndex(file, size, false)
}

func loadChecksummedIndex(file *os.File, size int64, compressed bool) (*LocationIndex, error) {
	if _, err := file.Seek(8, io.SeekStart); err != nil {
		return nil, err
	}
	bodySize := size - 8 - 4
	if bodySize < 0 {
		return nil, ErrCorruptIndex
	}
	bodyChecksum := crc32.NewIEEE()
	var err error
	buffered := bufio.NewReader(io.TeeReader(io.NewSectionReader(file, 8, bodySize), bodyChecksum))

	var persistedPrecision uint32
	if err := binary.Read(buffered, binary.BigEndian, &persistedPrecision); err != nil {
		return nil, err
	}
	var persistedHotThreshold uint32
	if err := binary.Read(buffered, binary.BigEndian, &persistedHotThreshold); err != nil {
		return nil, err
	}

	var count uint32
	if err := binary.Read(buffered, binary.BigEndian, &count); err != nil {
		return nil, err
	}

	idx := NewLocationIndexWithOptions(IndexOptions{
		SpatialCellPrecision:    uint(persistedPrecision),
		HotSpatialCellThreshold: int(persistedHotThreshold),
	})
	for i := uint32(0); i < count; i++ {
		var persistedDocID uint32
		if err := binary.Read(buffered, binary.BigEndian, &persistedDocID); err != nil {
			return nil, err
		}
		record, err := readRecord(buffered)
		if err != nil {
			return nil, err
		}
		decoded, err := readDecodedLocation(buffered)
		if err != nil {
			return nil, err
		}
		docID := docID(persistedDocID)
		idx.Records[record.ID] = record
		idx.docIDsByRecord[record.ID] = docID
		idx.recordsByDocID[docID] = record.ID
		idx.decodedRecords[docID] = decoded
		if docID > idx.nextDocID {
			idx.nextDocID = docID
		}
	}

	var byPayload map[string]Set[docID]
	var byLabel map[Label]Set[docID]
	var byPayloadPrefix map[string]Set[docID]
	var bySpatialPrefix map[string]Set[docID]
	var bySpatialCell map[string]Set[docID]
	var byRefinedSpatialCell map[string]Set[docID]
	var byHotSpatialFallback map[string]Set[docID]
	if compressed {
		byPayload, err = readDocIDSetMap(buffered)
	} else {
		byPayload, err = readDocIDSetMapRaw(buffered)
	}
	if err != nil {
		return nil, err
	}
	if compressed {
		byLabel, err = readLabelDocIDSetMap(buffered)
	} else {
		byLabel, err = readLabelDocIDSetMapRaw(buffered)
	}
	if err != nil {
		return nil, err
	}
	if compressed {
		byPayloadPrefix, err = readDocIDSetMap(buffered)
	} else {
		byPayloadPrefix, err = readDocIDSetMapRaw(buffered)
	}
	if err != nil {
		return nil, err
	}
	if compressed {
		bySpatialPrefix, err = readDocIDSetMap(buffered)
	} else {
		bySpatialPrefix, err = readDocIDSetMapRaw(buffered)
	}
	if err != nil {
		return nil, err
	}
	if compressed {
		bySpatialCell, err = readDocIDSetMap(buffered)
	} else {
		bySpatialCell, err = readDocIDSetMapRaw(buffered)
	}
	if err != nil {
		return nil, err
	}
	if compressed {
		byRefinedSpatialCell, err = readDocIDSetMap(buffered)
	} else {
		byRefinedSpatialCell, err = readDocIDSetMapRaw(buffered)
	}
	if err != nil {
		return nil, err
	}
	if compressed {
		byHotSpatialFallback, err = readDocIDSetMap(buffered)
	} else {
		byHotSpatialFallback, err = readDocIDSetMapRaw(buffered)
	}
	if err != nil {
		return nil, err
	}
	hotSpatialCells, err := readStringSet(buffered)
	if err != nil {
		return nil, err
	}

	idx.ByPayload = byPayload
	idx.ByLabel = byLabel
	idx.ByPayloadPrefix = byPayloadPrefix
	idx.bySpatialPrefix = bySpatialPrefix
	idx.bySpatialCell = bySpatialCell
	idx.byRefinedSpatialCell = byRefinedSpatialCell
	idx.byHotSpatialFallback = byHotSpatialFallback
	idx.hotSpatialCells = hotSpatialCells

	if _, err := file.Seek(size-4, io.SeekStart); err != nil {
		return nil, err
	}
	var expectedChecksum uint32
	if err := binary.Read(file, binary.BigEndian, &expectedChecksum); err != nil {
		return nil, err
	}
	if bodyChecksum.Sum32() != expectedChecksum {
		return nil, ErrCorruptIndex
	}
	if err := idx.validateLoadedState(); err != nil {
		return nil, err
	}

	return idx, nil
}

func loadIndexV2(reader io.Reader) (*LocationIndex, error) {
	idx, err := loadIndexV1Body(reader, true)
	if err != nil {
		return nil, err
	}
	return idx, idx.validateLoadedState()
}

func loadIndexV1(reader io.Reader) (*LocationIndex, error) {
	idx, err := loadIndexV1Body(reader, false)
	if err != nil {
		return nil, err
	}
	return idx, idx.validateLoadedState()
}

func loadIndexV1Body(reader io.Reader, withOptions bool) (*LocationIndex, error) {
	options := DefaultIndexOptions()
	if withOptions {
		var persistedPrecision uint32
		if err := binary.Read(reader, binary.BigEndian, &persistedPrecision); err != nil {
			return nil, err
		}
		var persistedHotThreshold uint32
		if err := binary.Read(reader, binary.BigEndian, &persistedHotThreshold); err != nil {
			return nil, err
		}
		options = IndexOptions{SpatialCellPrecision: uint(persistedPrecision), HotSpatialCellThreshold: int(persistedHotThreshold)}
	}

	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, err
	}

	idx := NewLocationIndexWithOptions(options)
	for i := uint32(0); i < count; i++ {
		record, err := readRecord(reader)
		if err != nil {
			return nil, err
		}
		if err := idx.Insert(record); err != nil {
			return nil, err
		}
	}

	return idx, nil
}

func (idx *LocationIndex) validateLoadedState() error {
	for recordID, docID := range idx.docIDsByRecord {
		if _, ok := idx.Records[recordID]; !ok {
			return ErrCorruptIndex
		}
		if mappedRecordID, ok := idx.recordsByDocID[docID]; !ok || mappedRecordID != recordID {
			return ErrCorruptIndex
		}
		if _, ok := idx.decodedRecords[docID]; !ok {
			return ErrCorruptIndex
		}
	}
	for docID, recordID := range idx.recordsByDocID {
		if mappedDocID, ok := idx.docIDsByRecord[recordID]; !ok || mappedDocID != docID {
			return ErrCorruptIndex
		}
	}
	for _, sets := range []map[string]Set[docID]{idx.ByPayload, idx.ByPayloadPrefix, idx.bySpatialPrefix, idx.bySpatialCell, idx.byRefinedSpatialCell, idx.byHotSpatialFallback} {
		for _, set := range sets {
			for docID := range set {
				if _, ok := idx.recordsByDocID[docID]; !ok {
					return ErrCorruptIndex
				}
			}
		}
	}
	for _, set := range idx.ByLabel {
		for docID := range set {
			if _, ok := idx.recordsByDocID[docID]; !ok {
				return ErrCorruptIndex
			}
		}
	}
	return nil
}

func writeDecodedLocation(writer io.Writer, decoded locationid.DecodedLocation) error {
	if err := binary.Write(writer, binary.BigEndian, uint32(decoded.Precision)); err != nil {
		return err
	}
	for _, value := range []float64{decoded.Bounds.MinLat, decoded.Bounds.MaxLat, decoded.Bounds.MinLon, decoded.Bounds.MaxLon, decoded.CenterLat, decoded.CenterLon} {
		if err := binary.Write(writer, binary.BigEndian, value); err != nil {
			return err
		}
	}
	return nil
}

func readDecodedLocation(reader io.Reader) (locationid.DecodedLocation, error) {
	var precision uint32
	if err := binary.Read(reader, binary.BigEndian, &precision); err != nil {
		return locationid.DecodedLocation{}, err
	}
	values := make([]float64, 6)
	for i := range values {
		if err := binary.Read(reader, binary.BigEndian, &values[i]); err != nil {
			return locationid.DecodedLocation{}, err
		}
	}
	return locationid.DecodedLocation{
		Precision: uint(precision),
		Bounds: locationid.Bounds{
			MinLat: values[0],
			MaxLat: values[1],
			MinLon: values[2],
			MaxLon: values[3],
		},
		CenterLat: values[4],
		CenterLon: values[5],
	}, nil
}

func writeDocIDSetMap(writer io.Writer, values map[string]Set[docID]) error {
	keys := sortedStringKeys(values)
	if err := binary.Write(writer, binary.BigEndian, uint32(len(keys))); err != nil {
		return err
	}
	for _, key := range keys {
		if err := writeString(writer, key); err != nil {
			return err
		}
		if err := writeDocIDSet(writer, values[key]); err != nil {
			return err
		}
	}
	return nil
}

func readDocIDSetMap(reader io.Reader) (map[string]Set[docID], error) {
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	values := make(map[string]Set[docID], count)
	for i := uint32(0); i < count; i++ {
		key, err := readString(reader)
		if err != nil {
			return nil, err
		}
		set, err := readDocIDSet(reader)
		if err != nil {
			return nil, err
		}
		values[key] = set
	}
	return values, nil
}

func readDocIDSetMapRaw(reader io.Reader) (map[string]Set[docID], error) {
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	values := make(map[string]Set[docID], count)
	for i := uint32(0); i < count; i++ {
		key, err := readString(reader)
		if err != nil {
			return nil, err
		}
		set, err := readDocIDSetRaw(reader)
		if err != nil {
			return nil, err
		}
		values[key] = set
	}
	return values, nil
}

func writeLabelDocIDSetMap(writer io.Writer, values map[Label]Set[docID]) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	if err := binary.Write(writer, binary.BigEndian, uint32(len(keys))); err != nil {
		return err
	}
	for _, key := range keys {
		if err := writeString(writer, key); err != nil {
			return err
		}
		if err := writeDocIDSet(writer, values[Label(key)]); err != nil {
			return err
		}
	}
	return nil
}

func readLabelDocIDSetMap(reader io.Reader) (map[Label]Set[docID], error) {
	values, err := readDocIDSetMap(reader)
	if err != nil {
		return nil, err
	}
	labels := make(map[Label]Set[docID], len(values))
	for key, set := range values {
		labels[Label(key)] = set
	}
	return labels, nil
}

func readLabelDocIDSetMapRaw(reader io.Reader) (map[Label]Set[docID], error) {
	values, err := readDocIDSetMapRaw(reader)
	if err != nil {
		return nil, err
	}
	labels := make(map[Label]Set[docID], len(values))
	for key, set := range values {
		labels[Label(key)] = set
	}
	return labels, nil
}

func writeDocIDSet(writer io.Writer, values Set[docID]) error {
	ordered := sortedDocIDs(values)
	if err := binary.Write(writer, binary.BigEndian, uint32(len(ordered))); err != nil {
		return err
	}
	var previous uint64
	var buffer [10]byte
	for _, id := range ordered {
		delta := uint64(id) - previous
		n := binary.PutUvarint(buffer[:], delta)
		if _, err := writer.Write(buffer[:n]); err != nil {
			return err
		}
		previous = uint64(id)
	}
	return nil
}

func readDocIDSet(reader io.Reader) (Set[docID], error) {
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	values := NewSetSize[docID](int(count))
	var previous uint64
	byteReader, ok := reader.(io.ByteReader)
	if !ok {
		byteReader = &readerByteAdapter{reader: reader}
	}
	for i := uint32(0); i < count; i++ {
		delta, err := binary.ReadUvarint(byteReader)
		if err != nil {
			return nil, err
		}
		previous += delta
		values.Add(docID(previous))
	}
	return values, nil
}

func readDocIDSetRaw(reader io.Reader) (Set[docID], error) {
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	values := NewSetSize[docID](int(count))
	for i := uint32(0); i < count; i++ {
		var id uint32
		if err := binary.Read(reader, binary.BigEndian, &id); err != nil {
			return nil, err
		}
		values.Add(docID(id))
	}
	return values, nil
}

type readerByteAdapter struct {
	reader io.Reader
}

func (r *readerByteAdapter) ReadByte() (byte, error) {
	var buffer [1]byte
	_, err := io.ReadFull(r.reader, buffer[:])
	return buffer[0], err
}

func writeStringSet(writer io.Writer, values map[string]struct{}) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := binary.Write(writer, binary.BigEndian, uint32(len(keys))); err != nil {
		return err
	}
	for _, key := range keys {
		if err := writeString(writer, key); err != nil {
			return err
		}
	}
	return nil
}

func readStringSet(reader io.Reader) (map[string]struct{}, error) {
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	values := make(map[string]struct{}, count)
	for i := uint32(0); i < count; i++ {
		key, err := readString(reader)
		if err != nil {
			return nil, err
		}
		values[key] = struct{}{}
	}
	return values, nil
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

func sortedStringKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func countDocIDSetMapEntries(values map[string]Set[docID]) int {
	total := 0
	for _, set := range values {
		total += len(set)
	}
	return total
}

func countLabelDocIDSetMapEntries(values map[Label]Set[docID]) int {
	total := 0
	for _, set := range values {
		total += len(set)
	}
	return total
}
