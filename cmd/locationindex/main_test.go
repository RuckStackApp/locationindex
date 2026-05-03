package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	locationid "github.com/ruckstackapp/locationid/go"
)

func TestCreateAddAndSearchPrefix(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.lidx")

	createOutput := runCLI(t, "create", "--index", indexPath, "--cell-precision", "14")
	if createOutput["status"] != "created" {
		t.Fatalf("create status = %v, want created", createOutput["status"])
	}

	code := mustCode(t, 37.7749, -122.4194, 12)
	addOutput := runCLI(t,
		"add",
		"--index", indexPath,
		"--cell-precision", "14",
		"--id", "sf-museum",
		"--code", code,
		"--label", "museum",
		"--label", "landmark",
		"--metadata", "name=Example Museum",
	)

	if addOutput["status"] != "added" {
		t.Fatalf("add status = %v, want added", addOutput["status"])
	}

	record := addOutput["record"].(map[string]any)
	payload := record["payload"].(string)
	searchOutput := runCLI(t,
		"search", "prefix",
		"--index", indexPath,
		"--prefix", payload[:4],
		"--label", "museum",
		"--limit", "1",
	)

	stats := searchOutput["stats"].(map[string]any)
	if _, ok := stats["candidate_count"]; ok {
		t.Fatalf("did not expect candidate_count in fast prefix mode")
	}
	if stats["has_more"] != nil {
		t.Fatalf("did not expect has_more for single match prefix search")
	}

	records := searchOutput["records"].([]any)
	if len(records) != 1 {
		t.Fatalf("records len = %d, want 1", len(records))
	}
	if records[0].(map[string]any)["id"] != "sf-museum" {
		t.Fatalf("record id = %v, want sf-museum", records[0].(map[string]any)["id"])
	}
}

func TestSearchRadiusAndNearestReturnStats(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.lidx")
	runCLI(t, "create", "--index", indexPath, "--cell-precision", "14")

	for _, item := range []struct {
		id   string
		lat  float64
		lon  float64
		kind string
	}{
		{id: "near", lat: 37.7750, lon: -122.4195, kind: "food"},
		{id: "mid", lat: 37.7790, lon: -122.4180, kind: "food"},
		{id: "far", lat: 37.8044, lon: -122.2711, kind: "food"},
	} {
		runCLI(t,
			"add",
			"--index", indexPath,
			"--cell-precision", "14",
			"--id", item.id,
			"--code", mustCode(t, item.lat, item.lon, 14),
			"--label", item.kind,
		)
	}

	radiusOutput := runCLI(t,
		"search", "radius",
		"--index", indexPath,
		"--lat", "37.7749",
		"--lon", "-122.4194",
		"--radius", "1000",
		"--precision", "10",
		"--label", "food",
	)

	radiusStats := radiusOutput["stats"].(map[string]any)
	if int(radiusStats["candidate_count"].(float64)) < 2 {
		t.Fatalf("radius candidate_count = %v, want at least 2", radiusStats["candidate_count"])
	}
	if int(radiusStats["result_count"].(float64)) != 2 {
		t.Fatalf("radius result_count = %v, want 2", radiusStats["result_count"])
	}

	radiusResults := radiusOutput["results"].([]any)
	if len(radiusResults) != 2 {
		t.Fatalf("radius results len = %d, want 2", len(radiusResults))
	}

	nearestOutput := runCLI(t,
		"search", "nearest",
		"--index", indexPath,
		"--lat", "37.7749",
		"--lon", "-122.4194",
		"--limit", "2",
		"--label", "food",
	)

	nearestStats := nearestOutput["stats"].(map[string]any)
	if int(nearestStats["result_count"].(float64)) != 2 {
		t.Fatalf("nearest result_count = %v, want 2", nearestStats["result_count"])
	}
	if int(nearestStats["expansion_count"].(float64)) < 1 {
		t.Fatalf("nearest expansion_count = %v, want >= 1", nearestStats["expansion_count"])
	}
}

func runCLI(t *testing.T, args ...string) map[string]any {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("run(%v) error = %v, stderr = %q", args, err, stderr.String())
	}

	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", stdout.String(), err)
	}

	return decoded
}

func mustCode(t *testing.T, lat, lon float64, precision uint) string {
	t.Helper()

	code, err := locationid.Encode(lat, lon, precision)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	return code.String()
}
