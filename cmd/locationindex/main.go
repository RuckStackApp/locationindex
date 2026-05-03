package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ruckstackapp/locationindex"
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
	case "create":
		return runCreate(args[1:], stdout)
	case "add":
		return runAdd(args[1:], stdout)
	case "search":
		return runSearch(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		writeUsage(stdout)
		return nil
	default:
		writeUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runCreate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	indexPath := fs.String("index", "", "path to index file")
	cellPrecision := fs.Uint("cell-precision", locationindex.DefaultIndexOptions().SpatialCellPrecision, "spatial cell precision")
	hotThreshold := fs.Int("hot-threshold", locationindex.DefaultIndexOptions().HotSpatialCellThreshold, "hot spatial cell threshold")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *indexPath == "" {
		return errors.New("missing --index")
	}
	idx := locationindex.NewLocationIndexWithOptions(locationindex.IndexOptions{
		SpatialCellPrecision:    *cellPrecision,
		HotSpatialCellThreshold: *hotThreshold,
	})
	if err := idx.ValidateOptions(); err != nil {
		return err
	}
	if err := idx.Save(*indexPath); err != nil {
		return err
	}

	return writeJSON(stdout, map[string]any{
		"status":       "created",
		"index":        *indexPath,
		"record_count": 0,
	})
}

func runAdd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	indexPath := fs.String("index", "", "path to index file")
	id := fs.String("id", "", "record id")
	code := fs.String("code", "", "location code")
	cellPrecision := fs.Uint("cell-precision", locationindex.DefaultIndexOptions().SpatialCellPrecision, "spatial cell precision for new index")
	hotThreshold := fs.Int("hot-threshold", locationindex.DefaultIndexOptions().HotSpatialCellThreshold, "hot spatial cell threshold for new index")
	labels := stringListFlag{}
	metadata := stringListFlag{}
	fs.Var(&labels, "label", "repeatable label")
	fs.Var(&metadata, "metadata", "repeatable key=value metadata")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *indexPath == "" {
		return errors.New("missing --index")
	}
	idx, err := loadOrCreateIndex(*indexPath, locationindex.IndexOptions{SpatialCellPrecision: *cellPrecision, HotSpatialCellThreshold: *hotThreshold})
	if err != nil {
		return err
	}

	record := locationindex.IndexedRecord{
		ID:       locationindex.RecordID(*id),
		Code:     *code,
		Labels:   labels.toLabels(),
		Metadata: parseMetadata(metadata),
	}
	if err := idx.Insert(record); err != nil {
		return err
	}

	if err := idx.Save(*indexPath); err != nil {
		return err
	}

	stored, _ := idx.GetByID(record.ID)
	return writeJSON(stdout, map[string]any{
		"status":       "added",
		"index":        *indexPath,
		"record_count": len(idx.Records),
		"record":       stored,
	})
}

func runSearch(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		writeSearchUsage(stderr)
		return errors.New("missing search type")
	}

	switch args[0] {
	case "prefix":
		return runSearchPrefix(args[1:], stdout)
	case "bbox":
		return runSearchBoundingBox(args[1:], stdout)
	case "radius":
		return runSearchRadius(args[1:], stdout)
	case "nearest":
		return runSearchNearest(args[1:], stdout)
	default:
		writeSearchUsage(stderr)
		return fmt.Errorf("unknown search type %q", args[0])
	}
}

func runSearchPrefix(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("search prefix", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	indexPath := fs.String("index", "", "path to index file")
	prefix := fs.String("prefix", "", "payload prefix")
	labels := stringListFlag{}
	limit := fs.Int("limit", 0, "max records")
	countExact := fs.Bool("count-exact", false, "compute exact candidate count")
	fs.Var(&labels, "label", "repeatable label")
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexRequired(*indexPath)
	if err != nil {
		return err
	}

	response := idx.SearchByPrefixDetailed(*prefix, locationindex.QueryOptions{Labels: labels.toLabels(), Limit: *limit, ExactCandidateCount: *countExact})
	return writeJSON(stdout, response)
}

func runSearchBoundingBox(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("search bbox", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	indexPath := fs.String("index", "", "path to index file")
	minLat := fs.Float64("min-lat", 0, "minimum latitude")
	maxLat := fs.Float64("max-lat", 0, "maximum latitude")
	minLon := fs.Float64("min-lon", 0, "minimum longitude")
	maxLon := fs.Float64("max-lon", 0, "maximum longitude")
	precision := fs.Uint("precision", 8, "search precision")
	limit := fs.Int("limit", 0, "max records")
	labels := stringListFlag{}
	fs.Var(&labels, "label", "repeatable label")
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexRequired(*indexPath)
	if err != nil {
		return err
	}

	response := idx.SearchBoundingBoxDetailed(locationindex.BoundingBox{
		MinLat: *minLat,
		MaxLat: *maxLat,
		MinLon: *minLon,
		MaxLon: *maxLon,
	}, uint(*precision), locationindex.QueryOptions{Labels: labels.toLabels(), Limit: *limit})
	return writeJSON(stdout, response)
}

func runSearchRadius(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("search radius", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	indexPath := fs.String("index", "", "path to index file")
	lat := fs.Float64("lat", 0, "latitude")
	lon := fs.Float64("lon", 0, "longitude")
	radius := fs.Float64("radius", 0, "radius meters")
	precision := fs.Uint("precision", 8, "search precision")
	limit := fs.Int("limit", 0, "max records")
	labels := stringListFlag{}
	fs.Var(&labels, "label", "repeatable label")
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexRequired(*indexPath)
	if err != nil {
		return err
	}

	response := idx.SearchRadiusDetailed(locationindex.RadiusQuery{
		Lat:          *lat,
		Lon:          *lon,
		RadiusMeters: *radius,
		Precision:    uint(*precision),
	}, locationindex.QueryOptions{Labels: labels.toLabels(), Limit: *limit})
	return writeJSON(stdout, response)
}

func runSearchNearest(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("search nearest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	indexPath := fs.String("index", "", "path to index file")
	lat := fs.Float64("lat", 0, "latitude")
	lon := fs.Float64("lon", 0, "longitude")
	limit := fs.Int("limit", 1, "max results")
	labels := stringListFlag{}
	fs.Var(&labels, "label", "repeatable label")
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexRequired(*indexPath)
	if err != nil {
		return err
	}

	response := idx.SearchNearestDetailed(*lat, *lon, *limit, locationindex.QueryOptions{Labels: labels.toLabels()})
	return writeJSON(stdout, response)
}

func loadOrCreateIndex(path string, options locationindex.IndexOptions) (*locationindex.LocationIndex, error) {
	idx, err := locationindex.Load(path)
	if err == nil {
		return idx, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		idx := locationindex.NewLocationIndexWithOptions(options)
		if err := idx.ValidateOptions(); err != nil {
			return nil, err
		}
		return idx, nil
	}
	return nil, err
}

func loadIndexRequired(path string) (*locationindex.LocationIndex, error) {
	if path == "" {
		return nil, errors.New("missing --index")
	}
	return locationindex.Load(path)
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: locationindex <create|add|search> [args]")
	fmt.Fprintln(w, "search types: prefix, bbox, radius, nearest")
}

func writeSearchUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: locationindex search <prefix|bbox|radius|nearest> [args]")
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func (f stringListFlag) toLabels() []locationindex.Label {
	labels := make([]locationindex.Label, 0, len(f))
	for _, value := range f {
		labels = append(labels, locationindex.Label(value))
	}
	return labels
}

func parseMetadata(values []string) map[string]string {
	if len(values) == 0 {
		return nil
	}

	metadata := make(map[string]string, len(values))
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 {
			metadata[value] = ""
			continue
		}
		metadata[parts[0]] = parts[1]
	}
	return metadata
}
