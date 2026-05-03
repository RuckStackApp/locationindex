package locationindex

import (
	"os"
	"path/filepath"
	"testing"

	locationid "github.com/ruckstackapp/locationid/go"
)

func TestInsertAndGetByID(t *testing.T) {
	idx := NewLocationIndex()
	record := mustRecord(t, "sf", 37.7749, -122.4194, 12, []Label{"city", "west"})

	if err := idx.Insert(record); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	got, ok := idx.GetByID(record.ID)
	if !ok {
		t.Fatalf("GetByID() did not find record")
	}

	if got.Payload == "" {
		t.Fatalf("expected payload to be populated")
	}
	docID, ok := idx.docIDsByRecord[record.ID]
	if !ok {
		t.Fatalf("expected docID mapping to contain record")
	}

	if _, ok := idx.ByPayload[got.Payload][docID]; !ok {
		t.Fatalf("expected payload index to contain record")
	}
	if _, ok := idx.decodedRecords[docID]; !ok {
		t.Fatalf("expected decoded record cache to contain record")
	}
	if len(idx.bySpatialCell) == 0 {
		t.Fatalf("expected spatial cell index to be populated")
	}

	for _, prefix := range prefixes(got.Payload) {
		if _, ok := idx.ByPayloadPrefix[prefix][docID]; !ok {
			t.Fatalf("expected prefix %q to contain record", prefix)
		}
	}

	if _, ok := idx.ByLabel["city"][docID]; !ok {
		t.Fatalf("expected label index to contain record")
	}
}

func TestInsertRejectsDuplicateID(t *testing.T) {
	idx := NewLocationIndex()
	record := mustRecord(t, "one", 37.7749, -122.4194, 12, nil)

	if err := idx.Insert(record); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	if err := idx.Insert(record); err != ErrDuplicateID {
		t.Fatalf("Insert() duplicate error = %v, want %v", err, ErrDuplicateID)
	}
}

func TestRemove(t *testing.T) {
	idx := NewLocationIndex()
	record := mustRecord(t, "remove-me", 37.7749, -122.4194, 12, []Label{"city"})

	if err := idx.Insert(record); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	if err := idx.Remove(record.ID); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	if _, ok := idx.GetByID(record.ID); ok {
		t.Fatalf("expected record to be removed")
	}

	if len(idx.ByPayload) != 0 || len(idx.ByPayloadPrefix) != 0 || len(idx.ByLabel) != 0 || len(idx.bySpatialCell) != 0 || len(idx.decodedRecords) != 0 {
		t.Fatalf("expected derived indexes to be cleaned up")
	}
}

func TestUpdate(t *testing.T) {
	idx := NewLocationIndex()
	original := mustRecord(t, "poi", 37.7749, -122.4194, 12, []Label{"city"})
	updated := mustRecord(t, "poi", 34.0522, -118.2437, 12, []Label{"city", "south"})

	if err := idx.Insert(original); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	if err := idx.Update(updated); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	got, ok := idx.GetByID(updated.ID)
	if !ok {
		t.Fatalf("expected updated record to exist")
	}

	if got.Payload != updated.Payload {
		t.Fatalf("updated payload = %q, want %q", got.Payload, updated.Payload)
	}

	docID, ok := idx.docIDsByRecord[updated.ID]
	if !ok {
		t.Fatalf("expected docID mapping for updated record")
	}
	if _, ok := idx.ByLabel["south"][docID]; !ok {
		t.Fatalf("expected new label index to contain record")
	}
}

func TestSearchByPrefixAndLabels(t *testing.T) {
	idx := NewLocationIndex()
	records := []IndexedRecord{
		mustRecord(t, "sf-museum", 37.8000, -122.4177, 12, []Label{"museum", "landmark"}),
		mustRecord(t, "sf-park", 37.7694, -122.4862, 12, []Label{"park"}),
		mustRecord(t, "la-museum", 34.0638, -118.3589, 12, []Label{"museum"}),
	}

	for _, record := range records {
		if err := idx.Insert(record); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	decoded, err := locationid.Decode(locationid.New(records[0].Code))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	results := idx.SearchByPrefix(decoded.Payload[:4], QueryOptions{Labels: []Label{"museum"}})
	if len(results) != 1 || results[0].ID != "sf-museum" {
		t.Fatalf("SearchByPrefix() = %#v, want sf-museum only", results)
	}
}

func TestSearchByPrefixLimitKeepsFullCandidateCount(t *testing.T) {
	idx := NewLocationIndex()
	records := []IndexedRecord{
		mustRecord(t, "one", 37.8000, -122.4177, 12, []Label{"museum"}),
		mustRecord(t, "two", 37.8001, -122.4178, 12, []Label{"museum"}),
	}

	for _, record := range records {
		if err := idx.Insert(record); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	decoded, err := locationid.Decode(locationid.New(records[0].Code))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	response := idx.SearchByPrefixDetailed(decoded.Payload[:4], QueryOptions{Labels: []Label{"museum"}, Limit: 1, ExactCandidateCount: true})
	if response.Stats.CandidateCount != 2 {
		t.Fatalf("candidate_count = %d, want 2", response.Stats.CandidateCount)
	}
	if !response.Stats.CandidateCountExact {
		t.Fatalf("expected exact candidate count")
	}
	if len(response.Records) != 1 {
		t.Fatalf("records len = %d, want 1", len(response.Records))
	}
}

func TestSearchByPrefixLimitFastModeHasMore(t *testing.T) {
	idx := NewLocationIndex()
	records := []IndexedRecord{
		mustRecord(t, "one", 37.8000, -122.4177, 12, []Label{"museum"}),
		mustRecord(t, "two", 37.8001, -122.4178, 12, []Label{"museum"}),
	}

	for _, record := range records {
		if err := idx.Insert(record); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	decoded, err := locationid.Decode(locationid.New(records[0].Code))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	response := idx.SearchByPrefixDetailed(decoded.Payload[:4], QueryOptions{Labels: []Label{"museum"}, Limit: 1})
	if response.Stats.CandidateCount != 0 {
		t.Fatalf("candidate_count = %d, want 0 in fast mode", response.Stats.CandidateCount)
	}
	if response.Stats.CandidateCountExact {
		t.Fatalf("did not expect exact candidate count")
	}
	if !response.Stats.HasMore {
		t.Fatalf("expected has_more in fast mode")
	}
}

func TestSearchBoundingBox(t *testing.T) {
	idx := NewLocationIndex()
	records := []IndexedRecord{
		mustRecord(t, "golden-gate", 37.8199, -122.4783, 12, []Label{"landmark"}),
		mustRecord(t, "ferry-building", 37.7955, -122.3937, 12, []Label{"landmark"}),
		mustRecord(t, "griffith", 34.1184, -118.3004, 12, []Label{"landmark"}),
	}

	for _, record := range records {
		if err := idx.Insert(record); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	results := idx.SearchBoundingBox(BoundingBox{
		MinLat: 37.70,
		MaxLat: 37.85,
		MinLon: -122.52,
		MaxLon: -122.35,
	}, 8, QueryOptions{})

	if len(results) != 2 {
		t.Fatalf("SearchBoundingBox() len = %d, want 2", len(results))
	}
}

func TestHotSpatialCellPromotion(t *testing.T) {
	original := HotSpatialCellThreshold()
	defer func() {
		if err := SetHotSpatialCellThreshold(original); err != nil {
			t.Fatalf("restore threshold error = %v", err)
		}
	}()

	if err := SetHotSpatialCellThreshold(1); err != nil {
		t.Fatalf("SetHotSpatialCellThreshold() error = %v", err)
	}

	idx := NewLocationIndex()
	records := []IndexedRecord{
		mustRecord(t, "one", 37.7749, -122.4194, 14, []Label{"a"}),
		mustRecord(t, "two", 37.7750, -122.4195, 14, []Label{"a"}),
	}

	for _, record := range records {
		if err := idx.Insert(record); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	if len(idx.hotSpatialCells) == 0 {
		t.Fatalf("expected at least one hot spatial cell")
	}
	if len(idx.byRefinedSpatialCell) == 0 {
		t.Fatalf("expected refined spatial postings to be populated")
	}
}

func TestHotSpatialCellFallbackForCoarseRecords(t *testing.T) {
	original := HotSpatialCellThreshold()
	defer func() {
		if err := SetHotSpatialCellThreshold(original); err != nil {
			t.Fatalf("restore threshold error = %v", err)
		}
	}()

	if err := SetHotSpatialCellThreshold(1); err != nil {
		t.Fatalf("SetHotSpatialCellThreshold() error = %v", err)
	}

	idx := NewLocationIndex()
	for _, record := range []IndexedRecord{
		mustRecord(t, "one", 37.7749, -122.4194, 12, []Label{"a"}),
		mustRecord(t, "two", 37.7750, -122.4195, 12, []Label{"a"}),
	} {
		if err := idx.Insert(record); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	if len(idx.hotSpatialCells) == 0 {
		t.Fatalf("expected at least one hot spatial cell")
	}
	if len(idx.byHotSpatialFallback) == 0 {
		t.Fatalf("expected hot spatial fallback postings to be populated")
	}
}

func TestCoverSpatialPrefixesForBox(t *testing.T) {
	box := BoundingBox{
		MinLat: 37.70,
		MaxLat: 37.85,
		MinLon: -122.52,
		MaxLon: -122.35,
	}

	prefixes := coverSpatialPrefixesForBox(box, 8, true)
	if len(prefixes) == 0 {
		t.Fatalf("coverSpatialPrefixesForBox() returned no prefixes")
	}

	for _, prefix := range prefixes {
		bounds := spatialPrefixBounds(prefix)
		if !intersects(box, bounds) {
			t.Fatalf("prefix %q does not intersect query box", prefix)
		}
	}
}

func TestSearchRadiusAndNearest(t *testing.T) {
	idx := NewLocationIndex()
	records := []IndexedRecord{
		mustRecord(t, "near", 37.7750, -122.4195, 14, []Label{"food"}),
		mustRecord(t, "mid", 37.7790, -122.4180, 14, []Label{"food"}),
		mustRecord(t, "far", 37.8044, -122.2711, 14, []Label{"food"}),
	}

	for _, record := range records {
		if err := idx.Insert(record); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	radiusResults := idx.SearchRadius(RadiusQuery{
		Lat:          37.7749,
		Lon:          -122.4194,
		RadiusMeters: 1000,
		Precision:    10,
	}, QueryOptions{Labels: []Label{"food"}})

	if len(radiusResults) != 2 {
		t.Fatalf("SearchRadius() len = %d, want 2", len(radiusResults))
	}

	if radiusResults[0].DistanceMeters > radiusResults[1].DistanceMeters {
		t.Fatalf("SearchRadius() results are not sorted by distance: %#v", radiusResults)
	}

	if !hasResultIDs(radiusResults, "near", "mid") {
		t.Fatalf("SearchRadius() IDs = %#v, want near and mid", radiusResults)
	}

	nearest := idx.SearchNearest(37.7749, -122.4194, 2, QueryOptions{Labels: []Label{"food"}})
	if len(nearest) != 2 {
		t.Fatalf("SearchNearest() len = %d, want 2", len(nearest))
	}

	if nearest[0].DistanceMeters > nearest[1].DistanceMeters {
		t.Fatalf("SearchNearest() results are not sorted by distance: %#v", nearest)
	}

	if !hasResultIDs(nearest, "near", "mid") {
		t.Fatalf("SearchNearest() IDs = %#v, want near and mid", nearest)
	}
}

func TestSaveAndLoad(t *testing.T) {
	idx := NewLocationIndex()
	records := []IndexedRecord{
		mustRecord(t, "one", 37.7749, -122.4194, 12, []Label{"a"}),
		mustRecord(t, "two", 34.0522, -118.2437, 12, []Label{"b"}),
	}

	for _, record := range records {
		if err := idx.Insert(record); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	path := filepath.Join(t.TempDir(), "index.lidx")
	if err := idx.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(loaded.Records) != len(records) {
		t.Fatalf("loaded record count = %d, want %d", len(loaded.Records), len(records))
	}
	if len(loaded.decodedRecords) != len(records) {
		t.Fatalf("decoded cache count = %d, want %d", len(loaded.decodedRecords), len(records))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if len(data) < 8 {
		t.Fatalf("persisted file too short: %d bytes", len(data))
	}
	if string(data[:4]) != string(persistedMagic[:]) {
		t.Fatalf("persisted magic = %q, want %q", string(data[:4]), string(persistedMagic[:]))
	}
}

func TestSpatialBitsAndBounds(t *testing.T) {
	record := mustRecord(t, "sf", 37.7749, -122.4194, 12, nil)
	decoded, err := locationid.Decode(locationid.New(record.Code))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	bits := spatialBits(decoded)
	if len(bits) != int(decoded.Precision*2) {
		t.Fatalf("spatialBits() len = %d, want %d", len(bits), decoded.Precision*2)
	}

	for _, prefix := range spatialPrefixes(decoded) {
		bounds := spatialPrefixBounds(prefix)
		if !intersects(BoundingBox{
			MinLat: decoded.Bounds.MinLat,
			MaxLat: decoded.Bounds.MaxLat,
			MinLon: decoded.Bounds.MinLon,
			MaxLon: decoded.Bounds.MaxLon,
		}, bounds) {
			t.Fatalf("spatial prefix bounds %q should contain record cell", prefix)
		}
	}
}

func TestSetSpatialCellPrecision(t *testing.T) {
	original := SpatialCellPrecision()
	defer func() {
		if err := SetSpatialCellPrecision(original); err != nil {
			t.Fatalf("restore precision error = %v", err)
		}
	}()

	if err := SetSpatialCellPrecision(15); err != nil {
		t.Fatalf("SetSpatialCellPrecision() error = %v", err)
	}
	if SpatialCellPrecision() != 15 {
		t.Fatalf("SpatialCellPrecision() = %d, want 15", SpatialCellPrecision())
	}
	if err := SetSpatialCellPrecision(0); err == nil {
		t.Fatalf("expected error for zero precision")
	}
}

func TestSetHotSpatialCellThreshold(t *testing.T) {
	original := HotSpatialCellThreshold()
	defer func() {
		if err := SetHotSpatialCellThreshold(original); err != nil {
			t.Fatalf("restore threshold error = %v", err)
		}
	}()

	if err := SetHotSpatialCellThreshold(2); err != nil {
		t.Fatalf("SetHotSpatialCellThreshold() error = %v", err)
	}
	if HotSpatialCellThreshold() != 2 {
		t.Fatalf("HotSpatialCellThreshold() = %d, want 2", HotSpatialCellThreshold())
	}
	if err := SetHotSpatialCellThreshold(0); err == nil {
		t.Fatalf("expected error for zero threshold")
	}
}

func mustRecord(t *testing.T, id string, lat, lon float64, precision uint, labels []Label) IndexedRecord {
	t.Helper()

	code, err := locationid.Encode(lat, lon, precision)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	decoded, err := locationid.Decode(code)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	return IndexedRecord{
		ID:      RecordID(id),
		Code:    code.String(),
		Payload: decoded.Payload,
		Labels:  labels,
		Metadata: map[string]string{
			"name": id,
		},
	}
}

func hasResultIDs(results []Result, wantA, wantB RecordID) bool {
	seen := map[RecordID]bool{}
	for _, result := range results {
		seen[result.Record.ID] = true
	}
	return seen[wantA] && seen[wantB]
}
