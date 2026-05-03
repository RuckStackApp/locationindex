package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	locationid "github.com/ruckstackapp/locationid/go"
	"locationindex"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		writeUsage(stderr)
		return errors.New("missing command")
	}

	switch args[0] {
	case "generate":
		return runGenerate(args[1:], stdout)
	case "build":
		return runBuild(args[1:], stdout)
	case "load":
		return runLoad(args[1:], stdout)
	case "query":
		return runQuery(args[1:], stdout)
	case "run":
		return runFull(args[1:], stdout)
	case "help", "-h", "--help":
		writeUsage(stdout)
		return nil
	default:
		writeUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runGenerate(args []string, stdout io.Writer) error {
	config, err := parseDatasetConfig(args)
	if err != nil {
		return err
	}
	if config.Output == "" {
		return errors.New("missing --output")
	}

	records, err := generateRecords(config)
	if err != nil {
		return err
	}

	if err := writeDataset(config.Output, records); err != nil {
		return err
	}

	return writeJSON(stdout, map[string]any{
		"command":      "generate",
		"output":       config.Output,
		"record_count": len(records),
		"distribution": config.Distribution,
		"seed":         config.Seed,
	})
}

func runBuild(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	datasetPath := fs.String("dataset", "", "path to dataset jsonl")
	indexPath := fs.String("index", "", "path to index file")
	cellPrecision := fs.Uint("cell-precision", locationindex.DefaultIndexOptions().SpatialCellPrecision, "spatial cell precision")
	hotThreshold := fs.Int("hot-threshold", locationindex.DefaultIndexOptions().HotSpatialCellThreshold, "hot spatial cell threshold")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *datasetPath == "" || *indexPath == "" {
		return errors.New("missing --dataset or --index")
	}
	records, err := readDataset(*datasetPath)
	if err != nil {
		return err
	}

	result, err := buildIndex(records, *indexPath, locationindex.IndexOptions{SpatialCellPrecision: *cellPrecision, HotSpatialCellThreshold: *hotThreshold})
	if err != nil {
		return err
	}

	result.Command = "build"
	return writeJSON(stdout, result)
}

func runLoad(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	indexPath := fs.String("index", "", "path to index file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *indexPath == "" {
		return errors.New("missing --index")
	}
	start := time.Now()
	idx, err := locationindex.Load(*indexPath)
	if err != nil {
		return err
	}
	elapsed := time.Since(start)

	stats := memoryStats()
	return writeJSON(stdout, loadResult{
		Command:        "load",
		Index:          *indexPath,
		RecordCount:    len(idx.Records),
		LoadDurationMS: elapsed.Milliseconds(),
		Memory:         stats,
	})
}

func runQuery(args []string, stdout io.Writer) error {
	config, err := parseQueryConfig(args)
	if err != nil {
		return err
	}
	if config.Index == "" {
		return errors.New("missing --index")
	}
	idx, err := locationindex.Load(config.Index)
	if err != nil {
		return err
	}

	result, err := benchmarkQueries(idx, config)
	if err != nil {
		return err
	}
	result.Command = "query"
	result.Index = config.Index
	return writeJSON(stdout, result)
}

func runFull(args []string, stdout io.Writer) error {
	config, err := parseFullConfig(args)
	if err != nil {
		return err
	}
	if config.Dataset.Output == "" || config.Query.Index == "" {
		return errors.New("missing --dataset or --index")
	}
	records, err := generateRecords(config.Dataset)
	if err != nil {
		return err
	}
	if err := writeDataset(config.Dataset.Output, records); err != nil {
		return err
	}

	buildResult, err := buildIndex(records, config.Query.Index, locationindex.IndexOptions{SpatialCellPrecision: config.Query.CellPrecision, HotSpatialCellThreshold: config.Query.HotThreshold})
	if err != nil {
		return err
	}

	start := time.Now()
	idx, err := locationindex.Load(config.Query.Index)
	if err != nil {
		return err
	}
	loadDuration := time.Since(start)

	queryResult, err := benchmarkQueries(idx, config.Query)
	if err != nil {
		return err
	}

	return writeJSON(stdout, fullRunResult{
		Command: "run",
		Dataset: datasetSummary{
			Path:         config.Dataset.Output,
			RecordCount:  len(records),
			Distribution: config.Dataset.Distribution,
			Seed:         config.Dataset.Seed,
		},
		Build: buildResult,
		Load: loadResult{
			Index:          config.Query.Index,
			RecordCount:    len(idx.Records),
			LoadDurationMS: loadDuration.Milliseconds(),
			Memory:         memoryStats(),
		},
		Query: queryResult,
	})
}

type datasetConfig struct {
	Output       string
	RecordCount  int
	Precision    uint
	Distribution string
	Seed         int64
}

type queryConfig struct {
	Index         string
	Count         int
	Mix           string
	Limit         int
	Seed          int64
	Precision     uint
	CellPrecision uint
	HotThreshold  int
	CountExact    bool
	RadiusM       float64
	BBoxDelta     float64
	LabelRate     float64
}

type fullConfig struct {
	Dataset datasetConfig
	Query   queryConfig
}

type datasetRecord struct {
	ID       string            `json:"id"`
	Lat      float64           `json:"lat"`
	Lon      float64           `json:"lon"`
	Code     string            `json:"code"`
	Labels   []string          `json:"labels,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type memorySnapshot struct {
	AllocBytes      uint64 `json:"alloc_bytes"`
	TotalAllocBytes uint64 `json:"total_alloc_bytes"`
	SysBytes        uint64 `json:"sys_bytes"`
	HeapInuseBytes  uint64 `json:"heap_inuse_bytes"`
	NumGC           uint32 `json:"num_gc"`
}

type buildResult struct {
	Command         string         `json:"command,omitempty"`
	Index           string         `json:"index"`
	RecordCount     int            `json:"record_count"`
	BuildDurationMS int64          `json:"build_duration_ms"`
	SaveDurationMS  int64          `json:"save_duration_ms"`
	IndexBytes      int64          `json:"index_bytes"`
	Memory          memorySnapshot `json:"memory"`
}

type loadResult struct {
	Command        string         `json:"command,omitempty"`
	Index          string         `json:"index"`
	RecordCount    int            `json:"record_count"`
	LoadDurationMS int64          `json:"load_duration_ms"`
	Memory         memorySnapshot `json:"memory"`
}

type queryResult struct {
	Command                 string         `json:"command,omitempty"`
	Index                   string         `json:"index,omitempty"`
	QueryCount              int            `json:"query_count"`
	Mix                     string         `json:"mix"`
	P50MS                   float64        `json:"p50_ms"`
	P95MS                   float64        `json:"p95_ms"`
	P99MS                   float64        `json:"p99_ms"`
	AverageMS               float64        `json:"average_ms"`
	AverageCandidates       float64        `json:"average_candidates"`
	AverageResults          float64        `json:"average_results"`
	AveragePrefixes         float64        `json:"average_prefixes"`
	AverageExpansions       float64        `json:"average_expansions"`
	AverageFinalRadius      float64        `json:"average_final_radius"`
	CountsByType            map[string]int `json:"counts_by_type"`
	Memory                  memorySnapshot `json:"memory"`
	Samples                 []querySample  `json:"samples,omitempty"`
	SampleResultWindowCount int            `json:"sample_result_window_count"`
}

type querySample struct {
	Type       string                    `json:"type"`
	DurationMS float64                   `json:"duration_ms"`
	Stats      locationindex.SearchStats `json:"stats"`
}

type datasetSummary struct {
	Path         string `json:"path"`
	RecordCount  int    `json:"record_count"`
	Distribution string `json:"distribution"`
	Seed         int64  `json:"seed"`
}

type fullRunResult struct {
	Command string         `json:"command"`
	Dataset datasetSummary `json:"dataset"`
	Build   buildResult    `json:"build"`
	Load    loadResult     `json:"load"`
	Query   queryResult    `json:"query"`
}

func parseDatasetConfig(args []string) (datasetConfig, error) {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := datasetConfig{}
	fs.StringVar(&config.Output, "output", "", "path to dataset jsonl")
	fs.IntVar(&config.RecordCount, "records", 10000, "number of records")
	fs.UintVar(&config.Precision, "precision", 12, "location code precision")
	fs.StringVar(&config.Distribution, "distribution", "clustered", "uniform or clustered")
	fs.Int64Var(&config.Seed, "seed", 1, "random seed")
	if err := fs.Parse(args); err != nil {
		return datasetConfig{}, err
	}
	if config.RecordCount <= 0 {
		return datasetConfig{}, errors.New("--records must be positive")
	}
	return config, nil
}

func parseQueryConfig(args []string) (queryConfig, error) {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := queryConfig{}
	fs.StringVar(&config.Index, "index", "", "path to index file")
	fs.IntVar(&config.Count, "count", 1000, "number of queries")
	fs.StringVar(&config.Mix, "mix", "mixed", "mixed, prefix, bbox, radius, nearest")
	fs.IntVar(&config.Limit, "limit", 20, "result limit")
	fs.Int64Var(&config.Seed, "seed", 1, "random seed")
	fs.UintVar(&config.Precision, "precision", 8, "bbox/radius precision")
	fs.UintVar(&config.CellPrecision, "cell-precision", locationindex.DefaultIndexOptions().SpatialCellPrecision, "spatial cell precision")
	fs.IntVar(&config.HotThreshold, "hot-threshold", locationindex.DefaultIndexOptions().HotSpatialCellThreshold, "hot spatial cell threshold")
	fs.BoolVar(&config.CountExact, "count-exact", false, "compute exact candidate counts for prefix queries")
	fs.Float64Var(&config.RadiusM, "radius-meters", 1000, "radius query size")
	fs.Float64Var(&config.BBoxDelta, "bbox-delta", 0.02, "half-width in degrees for bbox queries")
	fs.Float64Var(&config.LabelRate, "label-rate", 0.3, "fraction of queries with label filter")
	if err := fs.Parse(args); err != nil {
		return queryConfig{}, err
	}
	if config.Count <= 0 {
		return queryConfig{}, errors.New("--count must be positive")
	}
	return config, nil
}

func parseFullConfig(args []string) (fullConfig, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := fullConfig{}
	fs.StringVar(&config.Dataset.Output, "dataset", "", "path to dataset jsonl")
	fs.IntVar(&config.Dataset.RecordCount, "records", 10000, "number of records")
	fs.UintVar(&config.Dataset.Precision, "dataset-precision", 12, "location code precision for generation")
	fs.StringVar(&config.Dataset.Distribution, "distribution", "clustered", "uniform or clustered")
	fs.Int64Var(&config.Dataset.Seed, "seed", 1, "random seed")
	fs.StringVar(&config.Query.Index, "index", "", "path to index file")
	fs.IntVar(&config.Query.Count, "count", 1000, "number of queries")
	fs.StringVar(&config.Query.Mix, "mix", "mixed", "mixed, prefix, bbox, radius, nearest")
	fs.IntVar(&config.Query.Limit, "limit", 20, "result limit")
	fs.UintVar(&config.Query.Precision, "precision", 8, "bbox/radius precision")
	fs.UintVar(&config.Query.CellPrecision, "cell-precision", locationindex.DefaultIndexOptions().SpatialCellPrecision, "spatial cell precision")
	fs.IntVar(&config.Query.HotThreshold, "hot-threshold", locationindex.DefaultIndexOptions().HotSpatialCellThreshold, "hot spatial cell threshold")
	fs.BoolVar(&config.Query.CountExact, "count-exact", false, "compute exact candidate counts for prefix queries")
	fs.Float64Var(&config.Query.RadiusM, "radius-meters", 1000, "radius query size")
	fs.Float64Var(&config.Query.BBoxDelta, "bbox-delta", 0.02, "half-width in degrees for bbox queries")
	fs.Float64Var(&config.Query.LabelRate, "label-rate", 0.3, "fraction of queries with label filter")
	if err := fs.Parse(args); err != nil {
		return fullConfig{}, err
	}
	if config.Dataset.RecordCount <= 0 {
		return fullConfig{}, errors.New("--records must be positive")
	}
	if config.Query.Count <= 0 {
		return fullConfig{}, errors.New("--count must be positive")
	}
	config.Query.Seed = config.Dataset.Seed
	return config, nil
}

func generateRecords(config datasetConfig) ([]datasetRecord, error) {
	rng := rand.New(rand.NewSource(config.Seed))
	records := make([]datasetRecord, 0, config.RecordCount)
	for i := 0; i < config.RecordCount; i++ {
		lat, lon := samplePoint(rng, config.Distribution)
		code, err := locationid.Encode(lat, lon, config.Precision)
		if err != nil {
			return nil, err
		}
		records = append(records, datasetRecord{
			ID:     fmt.Sprintf("rec_%08d", i),
			Lat:    lat,
			Lon:    lon,
			Code:   code.String(),
			Labels: sampleLabels(rng, lat, lon),
			Metadata: map[string]string{
				"source": "bench",
			},
		})
	}
	return records, nil
}

func samplePoint(rng *rand.Rand, distribution string) (float64, float64) {
	switch strings.ToLower(distribution) {
	case "uniform":
		return rng.Float64()*180 - 90, rng.Float64()*360 - 180
	default:
		clusters := [][2]float64{{37.7749, -122.4194}, {34.0522, -118.2437}, {40.7128, -74.0060}, {51.5074, -0.1278}, {35.6762, 139.6503}}
		base := clusters[rng.Intn(len(clusters))]
		lat := clamp(base[0]+rng.NormFloat64()*0.15, -90, 90)
		lon := normalizeLon(base[1] + rng.NormFloat64()*0.15)
		return lat, lon
	}
}

func sampleLabels(rng *rand.Rand, lat, lon float64) []string {
	labels := make([]string, 0, 3)
	category := []string{"museum", "park", "food", "hotel", "landmark"}
	labels = append(labels, category[rng.Intn(len(category))])
	if lat >= 0 {
		labels = append(labels, "north")
	} else {
		labels = append(labels, "south")
	}
	if lon >= 0 {
		labels = append(labels, "east")
	} else {
		labels = append(labels, "west")
	}
	return labels
}

func writeDataset(path string, records []datasetRecord) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	return file.Sync()
}

func readDataset(path string) ([]datasetRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := bufio.NewScanner(file)
	buffer := make([]byte, 0, 1024*1024)
	reader.Buffer(buffer, 16*1024*1024)
	records := make([]datasetRecord, 0)
	for reader.Scan() {
		var record datasetRecord
		if err := json.Unmarshal(reader.Bytes(), &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := reader.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func buildIndex(records []datasetRecord, indexPath string, options locationindex.IndexOptions) (buildResult, error) {
	idx := locationindex.NewLocationIndexWithOptions(options)
	if err := idx.ValidateOptions(); err != nil {
		return buildResult{}, err
	}
	buildStart := time.Now()
	for _, record := range records {
		labels := make([]locationindex.Label, 0, len(record.Labels))
		for _, label := range record.Labels {
			labels = append(labels, locationindex.Label(label))
		}
		if err := idx.Insert(locationindex.IndexedRecord{
			ID:       locationindex.RecordID(record.ID),
			Code:     record.Code,
			Labels:   labels,
			Metadata: record.Metadata,
		}); err != nil {
			return buildResult{}, err
		}
	}
	buildDuration := time.Since(buildStart)

	saveStart := time.Now()
	if err := idx.Save(indexPath); err != nil {
		return buildResult{}, err
	}
	saveDuration := time.Since(saveStart)

	info, err := os.Stat(indexPath)
	if err != nil {
		return buildResult{}, err
	}

	return buildResult{
		Index:           indexPath,
		RecordCount:     len(records),
		BuildDurationMS: buildDuration.Milliseconds(),
		SaveDurationMS:  saveDuration.Milliseconds(),
		IndexBytes:      info.Size(),
		Memory:          memoryStats(),
	}, nil
}

func benchmarkQueries(idx *locationindex.LocationIndex, config queryConfig) (queryResult, error) {
	records := sortedRecords(idx)
	if len(records) == 0 {
		return queryResult{}, errors.New("index contains no records")
	}

	rng := rand.New(rand.NewSource(config.Seed))
	durations := make([]float64, 0, config.Count)
	countsByType := map[string]int{}
	var totalCandidates, totalResults, totalPrefixes, totalExpansions int
	var totalFinalRadius float64
	samples := make([]querySample, 0, min(config.Count, 10))

	for i := 0; i < config.Count; i++ {
		queryType := chooseQueryType(rng, config.Mix)
		countsByType[queryType]++
		anchor := records[rng.Intn(len(records))]
		start := time.Now()
		stats := executeBenchmarkQuery(idx, anchor, queryType, config, rng)
		durationMS := float64(time.Since(start).Microseconds()) / 1000.0

		durations = append(durations, durationMS)
		totalCandidates += stats.CandidateCount
		totalResults += stats.ResultCount
		totalPrefixes += stats.PrefixCount
		totalExpansions += stats.ExpansionCount
		totalFinalRadius += float64(stats.FinalRadius)

		if len(samples) < cap(samples) {
			samples = append(samples, querySample{Type: queryType, DurationMS: durationMS, Stats: stats})
		}
	}

	sort.Float64s(durations)
	count := float64(config.Count)
	return queryResult{
		QueryCount:              config.Count,
		Mix:                     config.Mix,
		P50MS:                   percentile(durations, 0.50),
		P95MS:                   percentile(durations, 0.95),
		P99MS:                   percentile(durations, 0.99),
		AverageMS:               average(durations),
		AverageCandidates:       float64(totalCandidates) / count,
		AverageResults:          float64(totalResults) / count,
		AveragePrefixes:         float64(totalPrefixes) / count,
		AverageExpansions:       float64(totalExpansions) / count,
		AverageFinalRadius:      totalFinalRadius / count,
		CountsByType:            countsByType,
		Memory:                  memoryStats(),
		Samples:                 samples,
		SampleResultWindowCount: len(samples),
	}, nil
}

type decodedRecord struct {
	Record   locationindex.IndexedRecord
	Decoded  locationid.DecodedLocation
	LabelRef locationindex.Label
}

func sortedRecords(idx *locationindex.LocationIndex) []decodedRecord {
	ids := make([]locationindex.RecordID, 0, len(idx.Records))
	for id := range idx.Records {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	records := make([]decodedRecord, 0, len(ids))
	for _, id := range ids {
		record := idx.Records[id]
		decoded, err := locationid.Decode(locationid.New(record.Code))
		if err != nil {
			continue
		}
		var label locationindex.Label
		if len(record.Labels) > 0 {
			label = record.Labels[0]
		}
		records = append(records, decodedRecord{Record: record, Decoded: decoded, LabelRef: label})
	}
	return records
}

func chooseQueryType(rng *rand.Rand, mix string) string {
	switch strings.ToLower(mix) {
	case "prefix", "bbox", "radius", "nearest":
		return strings.ToLower(mix)
	default:
		types := [4]string{"prefix", "bbox", "radius", "nearest"}
		return types[rng.Intn(len(types))]
	}
}

func executeBenchmarkQuery(idx *locationindex.LocationIndex, anchor decodedRecord, queryType string, config queryConfig, rng *rand.Rand) locationindex.SearchStats {
	options := locationindex.QueryOptions{Limit: config.Limit}
	if anchor.LabelRef != "" && rng.Float64() < config.LabelRate {
		options.Labels = []locationindex.Label{anchor.LabelRef}
	}
	if queryType == "prefix" {
		options.ExactCandidateCount = config.CountExact
	}

	switch queryType {
	case "prefix":
		payload := anchor.Record.Payload
		prefixLen := 2
		if len(payload) > 4 {
			prefixLen = 4
		}
		return idx.SearchByPrefixDetailed(payload[:prefixLen], options).Stats
	case "bbox":
		return idx.SearchBoundingBoxDetailed(locationindex.BoundingBox{
			MinLat: anchor.Decoded.CenterLat - config.BBoxDelta,
			MaxLat: anchor.Decoded.CenterLat + config.BBoxDelta,
			MinLon: anchor.Decoded.CenterLon - config.BBoxDelta,
			MaxLon: anchor.Decoded.CenterLon + config.BBoxDelta,
		}, config.Precision, options).Stats
	case "radius":
		return idx.SearchRadiusDetailed(locationindex.RadiusQuery{
			Lat:          anchor.Decoded.CenterLat,
			Lon:          anchor.Decoded.CenterLon,
			RadiusMeters: config.RadiusM,
			Precision:    config.Precision,
		}, options).Stats
	default:
		return idx.SearchNearestDetailed(anchor.Decoded.CenterLat, anchor.Decoded.CenterLon, max(config.Limit, 1), options).Stats
	}
}

func percentile(sortedValues []float64, p float64) float64 {
	if len(sortedValues) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(len(sortedValues))*p)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sortedValues) {
		idx = len(sortedValues) - 1
	}
	return sortedValues[idx]
}

func average(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func memoryStats() memorySnapshot {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return memorySnapshot{
		AllocBytes:      stats.Alloc,
		TotalAllocBytes: stats.TotalAlloc,
		SysBytes:        stats.Sys,
		HeapInuseBytes:  stats.HeapInuse,
		NumGC:           stats.NumGC,
	}
}

func writeUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: locationindex-bench <generate|build|load|query|run> [args]")
	fmt.Fprintln(w, "generate/build/load/query can be run separately; run executes the full flow")
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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

func normalizeLon(lon float64) float64 {
	for lon < -180 {
		lon += 360
	}
	for lon > 180 {
		lon -= 360
	}
	return lon
}
