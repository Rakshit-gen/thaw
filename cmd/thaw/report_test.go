package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportMediansAndChart(t *testing.T) {
	dir := t.TempDir()
	vm := `{"when":"2026-10-17T10:00:00Z","profiles":["s3-express"],
	 "bases":[{"env":"py-data","boot_ms":1000,"setup_ms":2000}],
	 "runs":[
	  {"env":"py-data","strategy":"lazy","profile":"s3-express","ok":true,"answered_ms":9000},
	  {"env":"py-data","strategy":"lazy","profile":"s3-express","ok":true,"answered_ms":11000},
	  {"env":"py-data","strategy":"thaw","profile":"s3-express","ok":false,"answered_ms":2500}]}`
	if err := os.WriteFile(filepath.Join(dir, "vm.json"), []byte(vm), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdReport(context.Background(), []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{
		"| py-data | boot + setup from scratch | 3.00 s |",
		"| py-data | lazy | 10.0 s |", // median of 9 s and 11 s
		"| 0/1 |",                     // the failed thaw run is counted, not hidden
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "answer-py-data.svg")); err != nil {
		t.Error(err)
	}
}
