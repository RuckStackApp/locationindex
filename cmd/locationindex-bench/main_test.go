package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestGenerateBuildLoadAndQuery(t *testing.T) {
	tempDir := t.TempDir()
	datasetPath := filepath.Join(tempDir, "bench.jsonl")
	indexPath := filepath.Join(tempDir, "bench.lidx")

	generated := runBenchCLI(t,
		"generate",
		"--output", datasetPath,
		"--records", "50",
		"--distribution", "clustered",
		"--seed", "42",
	)
	if int(generated["record_count"].(float64)) != 50 {
		t.Fatalf("generated record_count = %v, want 50", generated["record_count"])
	}

	built := runBenchCLI(t,
		"build",
		"--dataset", datasetPath,
		"--index", indexPath,
		"--cell-precision", "14",
	)
	if int(built["record_count"].(float64)) != 50 {
		t.Fatalf("built record_count = %v, want 50", built["record_count"])
	}

	loaded := runBenchCLI(t, "load", "--index", indexPath)
	if int(loaded["record_count"].(float64)) != 50 {
		t.Fatalf("loaded record_count = %v, want 50", loaded["record_count"])
	}

	queried := runBenchCLI(t,
		"query",
		"--index", indexPath,
		"--count", "20",
		"--mix", "mixed",
		"--seed", "7",
	)
	if int(queried["query_count"].(float64)) != 20 {
		t.Fatalf("query_count = %v, want 20", queried["query_count"])
	}
	if queried["counts_by_type"] == nil {
		t.Fatalf("expected counts_by_type in query output")
	}
}

func TestRunCommand(t *testing.T) {
	tempDir := t.TempDir()
	datasetPath := filepath.Join(tempDir, "run.jsonl")
	indexPath := filepath.Join(tempDir, "run.lidx")

	output := runBenchCLI(t,
		"run",
		"--dataset", datasetPath,
		"--index", indexPath,
		"--cell-precision", "14",
		"--records", "40",
		"--count", "10",
		"--mix", "radius",
		"--seed", "9",
	)

	build := output["build"].(map[string]any)
	if int(build["record_count"].(float64)) != 40 {
		t.Fatalf("build record_count = %v, want 40", build["record_count"])
	}

	query := output["query"].(map[string]any)
	if int(query["query_count"].(float64)) != 10 {
		t.Fatalf("query_count = %v, want 10", query["query_count"])
	}
	countsByType := query["counts_by_type"].(map[string]any)
	if int(countsByType["radius"].(float64)) != 10 {
		t.Fatalf("radius count = %v, want 10", countsByType["radius"])
	}
}

func TestPercentile(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	if got := percentile(values, 0.50); got != 3 {
		t.Fatalf("percentile 50 = %v, want 3", got)
	}
	if got := percentile(values, 0.95); got != 5 {
		t.Fatalf("percentile 95 = %v, want 5", got)
	}
}

func runBenchCLI(t *testing.T, args ...string) map[string]any {
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
