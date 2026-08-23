package huggingface

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func checkpointServer(t *testing.T, h http.HandlerFunc) *Store {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Store{BaseURL: srv.URL, HTTP: srv.Client()}
}

func describe(spec string) *provisionerv1.DescribeModelRequest {
	return &provisionerv1.DescribeModelRequest{ModelSpec: spec}
}

// The size that matters is what the engine fetches, which is safetensors
// alone. A repository publishing the same weights as PyTorch, TensorFlow and
// Flax reports all of them in its tree; counting those overstates the
// download by 10x on openai-community/gpt2, and vLLM never asks for them.
func TestCheckpointCountsSafetensorsOnly(t *testing.T) {
	s := checkpointServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[
		  {"type":"file","path":"model-00001-of-00002.safetensors","size":140,"lfs":{"size":5000}},
		  {"type":"file","path":"model-00002-of-00002.safetensors","size":140,"lfs":{"size":3000}},
		  {"type":"file","path":"pytorch_model.bin","size":140,"lfs":{"size":900000}},
		  {"type":"file","path":"tf_model.h5","size":140,"lfs":{"size":900000}},
		  {"type":"file","path":"config.json","size":700},
		  {"type":"directory","path":"onnx"}
		]`)
	})
	got, err := s.Checkpoint(context.Background(), describe("org/model"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDownloadBytes() != 8000 {
		t.Fatalf("download_bytes = %d, want 8000 (safetensors only)", got.GetDownloadBytes())
	}
	if got.GetFileCount() != 2 {
		t.Fatalf("file_count = %d, want 2", got.GetFileCount())
	}
	if got.GetLargestFileBytes() != 5000 {
		t.Fatalf("largest_file_bytes = %d, want 5000", got.GetLargestFileBytes())
	}
}

// A git-lfs entry reports the pointer's size at the top level and the real
// size under lfs. Every weight file is lfs-backed, so reading the outer field
// would size a 474 GB checkpoint at a few kilobytes.
func TestCheckpointPrefersLFSSize(t *testing.T) {
	s := checkpointServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"type":"file","path":"model.safetensors","size":135,"lfs":{"size":26460000000}}]`)
	})
	got, err := s.Checkpoint(context.Background(), describe("org/model"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDownloadBytes() != 26460000000 {
		t.Fatalf("download_bytes = %d, want the lfs size not the pointer size", got.GetDownloadBytes())
	}
}

// A repository big enough for any of this to matter is a repository the hub
// paginates. An unpaged read truncates exactly the checkpoints that need
// sizing, and the shortfall looks like a plausible number.
func TestCheckpointFollowsPagination(t *testing.T) {
	var base string
	s := checkpointServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/x?cursor=2>; rel="next"`, base))
			fmt.Fprint(w, `[{"type":"file","path":"a.safetensors","size":10,"lfs":{"size":1000}}]`)
		default:
			fmt.Fprint(w, `[{"type":"file","path":"b.safetensors","size":10,"lfs":{"size":2000}}]`)
		}
	})
	base = s.BaseURL

	got, err := s.Checkpoint(context.Background(), describe("org/model"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDownloadBytes() != 3000 {
		t.Fatalf("download_bytes = %d, want 3000; the second page was dropped", got.GetDownloadBytes())
	}
	if got.GetFileCount() != 2 {
		t.Fatalf("file_count = %d, want 2", got.GetFileCount())
	}
}

// An unreadable listing must be an error, never a zero checkpoint. A caller
// projecting a download reads zero bytes as already finished.
func TestCheckpointErrorsRatherThanReportingZero(t *testing.T) {
	s := checkpointServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"gated"}`)
	})
	if _, err := s.Checkpoint(context.Background(), describe("org/model")); err == nil {
		t.Fatal("an unreadable listing returned no error")
	}
}

// A model published without safetensors is not a failure. Zero files with
// zero bytes is the accurate description, and the caller decides.
func TestCheckpointReportsEmptyForNoSafetensors(t *testing.T) {
	s := checkpointServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"type":"file","path":"pytorch_model.bin","size":140,"lfs":{"size":900000}}]`)
	})
	got, err := s.Checkpoint(context.Background(), describe("org/model"))
	if err != nil {
		t.Fatalf("a safetensors-free repository was reported as an error: %v", err)
	}
	if got.GetFileCount() != 0 || got.GetDownloadBytes() != 0 {
		t.Fatalf("got %+v, want an empty checkpoint", got)
	}
}

// The two reads must resolve one spec to one revision. Sizing against a
// different revision than the shape was read from describes two checkpoints
// while appearing to describe one.
func TestCheckpointHonoursTheRevisionInTheSpec(t *testing.T) {
	var asked string
	s := checkpointServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		fmt.Fprint(w, `[]`)
	})
	if _, err := s.Checkpoint(context.Background(), describe("org/model:v2")); err != nil {
		t.Fatal(err)
	}
	if want := "/api/models/org/model/tree/v2"; asked != want {
		t.Fatalf("asked for %q, want %q", asked, want)
	}
}

func TestCheckpointRejectsABadSpec(t *testing.T) {
	s := checkpointServer(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[]`) })
	if _, err := s.Checkpoint(context.Background(), describe("not-a-valid-spec")); err == nil {
		t.Fatal("a malformed spec was accepted")
	}
}

func TestNextPageURL(t *testing.T) {
	cases := map[string]string{
		"":                                 "",
		`<https://hf.co/next>; rel="next"`: "https://hf.co/next",
		`<https://hf.co/prev>; rel="prev"`: "",
		`<https://hf.co/a>; rel="prev", <https://hf.co/b>; rel="next"`: "https://hf.co/b",
	}
	for header, want := range cases {
		if got := nextPageURL(header); got != want {
			t.Fatalf("nextPageURL(%q) = %q, want %q", header, got, want)
		}
	}
}
