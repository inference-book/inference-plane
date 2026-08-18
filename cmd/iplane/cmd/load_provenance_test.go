package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

type fakeDescriber struct {
	dep       *provisionerv1.Deployment
	instances map[string]*provisionerv1.Instance
	depErr    error
	instErr   error
}

func (f *fakeDescriber) DescribeDeployment(_ context.Context, _ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
	if f.depErr != nil {
		return nil, f.depErr
	}
	return &provisionerv1.DescribeDeploymentResponse{Deployment: f.dep}, nil
}

func (f *fakeDescriber) DescribeInstance(_ context.Context, req *provisionerv1.DescribeInstanceRequest) (*provisionerv1.DescribeInstanceResponse, error) {
	if f.instErr != nil {
		return nil, f.instErr
	}
	return &provisionerv1.DescribeInstanceResponse{Instance: f.instances[req.GetId()]}, nil
}

func inst(provider, sku string, cards int32) *provisionerv1.Instance {
	return &provisionerv1.Instance{
		Provider: provider,
		Hardware: &provisionerv1.Hardware{GpuSku: sku, GpuCount: cards},
	}
}

func TestDescribeFleetNamesTheHardwareTheSweepRanOn(t *testing.T) {
	f := &fakeDescriber{
		dep: &provisionerv1.Deployment{
			Id:          "glm",
			InstanceIds: []string{"i-1"},
			EngineArgs:  []string{"--tensor-parallel-size", "4", "--model", "zai-org/GLM-5.2"},
		},
		instances: map[string]*provisionerv1.Instance{"i-1": inst("runpod", "B200", 4)},
	}

	got, err := describeFleet(context.Background(), f, f, "glm")
	if err != nil {
		t.Fatalf("describeFleet: %v", err)
	}
	want := fleetProvenance{Provider: "runpod", GPUSKU: "B200", GPUCount: 4, Replicas: 1, Plan: "tp4"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestDescribeFleetReportsEveryProviderInAHeterogeneousFleet is the case
// iplane exists for. Naming one provider would describe a run that did
// not happen.
func TestDescribeFleetReportsEveryProviderInAHeterogeneousFleet(t *testing.T) {
	f := &fakeDescriber{
		dep: &provisionerv1.Deployment{InstanceIds: []string{"i-1", "i-2", "i-3"}},
		instances: map[string]*provisionerv1.Instance{
			"i-1": inst("vast", "H100_SXM", 8),
			"i-2": inst("runpod", "B200", 8),
			"i-3": inst("vast", "H100_SXM", 8),
		},
	}

	got, err := describeFleet(context.Background(), f, f, "mixed")
	if err != nil {
		t.Fatalf("describeFleet: %v", err)
	}
	if got.Provider != "runpod+vast" {
		t.Errorf("provider = %q, want the distinct set joined and sorted", got.Provider)
	}
	if got.GPUSKU != "B200+H100_SXM" {
		t.Errorf("gpu_sku = %q, want both skus", got.GPUSKU)
	}
	if got.GPUCount != 24 {
		t.Errorf("gpu_count = %d, want 24 (the cards the run actually spanned)", got.GPUCount)
	}
	if got.Replicas != 3 {
		t.Errorf("replicas = %d, want 3", got.Replicas)
	}
}

// TestDescribeFleetFallsBackToTheSingularInstanceField covers v0.1-shaped
// records, which carry instance_id and no list.
func TestDescribeFleetFallsBackToTheSingularInstanceField(t *testing.T) {
	f := &fakeDescriber{
		dep:       &provisionerv1.Deployment{InstanceId: "i-legacy"},
		instances: map[string]*provisionerv1.Instance{"i-legacy": inst("runpod", "A100", 1)},
	}

	got, err := describeFleet(context.Background(), f, f, "old")
	if err != nil {
		t.Fatalf("describeFleet: %v", err)
	}
	if got.Replicas != 1 || got.Provider != "runpod" {
		t.Errorf("got %+v, want the singular field read as one replica", got)
	}
}

func TestDescribeFleetSurfacesAFailedRead(t *testing.T) {
	f := &fakeDescriber{depErr: errors.New("connection refused")}
	if _, err := describeFleet(context.Background(), f, f, "glm"); err == nil {
		t.Fatal("want an error when the deployment cannot be read")
	}
}

func TestPlanFromEngineArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"tp only", []string{"--tensor-parallel-size", "8"}, "tp8"},
		{"equals form", []string{"--tensor-parallel-size=4"}, "tp4"},
		{"tp and pp", []string{"--tensor-parallel-size", "4", "--pipeline-parallel-size", "2"}, "tp4pp2"},
		{"one way is no split", []string{"--tensor-parallel-size", "1"}, ""},
		{"absent", []string{"--model", "x"}, ""},
		{"unparseable", []string{"--tensor-parallel-size", "four"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planFromEngineArgs(tc.args); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSweepCSVColumnsMatchTheJSONFields is the property that lets a figure
// read either artifact and get the same columns. Reflection derives both,
// so this guards the derivation rather than a hand-written list.
func TestSweepCSVColumnsMatchTheJSONFields(t *testing.T) {
	cols := sweepCSVColumns()
	t.Logf("columns: %s", strings.Join(cols, ","))

	typ := reflect.TypeOf(sweepLevel{})
	if len(cols) != typ.NumField() {
		t.Fatalf("got %d columns for %d fields", len(cols), typ.NumField())
	}
	if cols[0] != "concurrency" {
		t.Errorf("first column = %q, want concurrency (the x axis)", cols[0])
	}

	// Every column has to be a real json field name, so a reader keyed by
	// the JSON schema finds each one.
	blob, err := json.Marshal(sweepLevel{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, c := range cols {
		if _, ok := m[c]; !ok {
			t.Errorf("column %q is not a json field of sweepLevel", c)
		}
	}
}

func TestWriteSweepCSVIsAPreambleThenARectangle(t *testing.T) {
	r := sweepReport{
		SchemaVersion:  sweepSchemaVersion,
		CapturedAt:     "2026-08-18T00:00:00Z",
		IplaneVersion:  "v0.4.0",
		Model:          "zai-org/GLM-5.2",
		Endpoint:       "http://localhost:8080",
		DeployID:       "glm",
		Fleet:          fleetProvenance{Provider: "runpod", GPUSKU: "B200", GPUCount: 4, Replicas: 1, Plan: "tp4"},
		PromptTokens:   8000,
		MaxTokens:      100,
		MeasureSeconds: 30,
		WindowSeconds:  3,
		Tolerance:      0.1,
		StableWindows:  3,
		Levels: []sweepLevel{
			{Concurrency: 1, TokensPerSec: 42.5, SteadyState: true},
			{Concurrency: 2, TokensPerSec: 80, SteadyState: false},
		},
	}

	var sb strings.Builder
	if err := writeSweepCSV(&sb, r); err != nil {
		t.Fatalf("writeSweepCSV: %v", err)
	}
	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")

	var comments, data []string
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			comments = append(comments, l)
			continue
		}
		data = append(data, l)
	}

	for _, want := range []string{
		"# schema_version 1",
		"# model zai-org/GLM-5.2",
		"# provider runpod",
		"# gpu_sku B200",
		"# gpu_count 4",
		"# plan tp4",
		"# prompt_tokens 8000",
	} {
		if !slicesContains(comments, want) {
			t.Errorf("preamble missing %q; got:\n%s", want, strings.Join(comments, "\n"))
		}
	}

	if len(data) != 3 {
		t.Fatalf("got %d data lines, want header plus two levels", len(data))
	}
	if data[0] != strings.Join(sweepCSVColumns(), ",") {
		t.Errorf("header = %q, want the derived column list", data[0])
	}
	// Every row has to be the same width as the header, or it is not a
	// table pgfplots can read.
	width := len(strings.Split(data[0], ","))
	for i, row := range data[1:] {
		if got := len(strings.Split(row, ",")); got != width {
			t.Errorf("row %d has %d fields, header has %d", i, got, width)
		}
	}
	if !strings.HasPrefix(data[1], "1,") {
		t.Errorf("first row = %q, want it to start at concurrency 1", data[1])
	}
	// 42.5 must survive as 42.5, and 80 must not become 80.0.
	if !strings.Contains(data[1], "42.5") {
		t.Errorf("row 1 lost the measured value: %q", data[1])
	}
	if strings.Contains(data[2], "80.0") {
		t.Errorf("row 2 grew a decimal point: %q", data[2])
	}
}

// TestEmptyFleetLeavesThePreambleSilent checks the bare-URL case. No
// hardware was asked about, so the file claims none.
func TestEmptyFleetLeavesThePreambleSilent(t *testing.T) {
	lines := sweepCSVPreamble(sweepReport{SchemaVersion: 1, Model: "mock/mock"})
	for _, l := range lines {
		for _, banned := range []string{"provider ", "gpu_sku ", "gpu_count ", "replicas ", "plan "} {
			if strings.HasPrefix(l, banned) {
				t.Errorf("preamble asserts %q with no fleet described", l)
			}
		}
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
