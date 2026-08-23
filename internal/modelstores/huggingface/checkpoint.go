package huggingface

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// How big is the download, as opposed to how big is the model.
//
// The architecture read answers the second question and cannot answer the
// first: a parameter count is a claim about tensors, and the bytes that have
// to cross a network before any tensor exists depend on how the repository
// chose to publish them. A four-bit checkpoint of a trillion-parameter model
// is 474 GB; the same model at fp16 is 1.5 TB; and #382 is the whole story of
// what happens when the two are confused.
//
// Read separately from Architecture, through its own optional interface, so
// only the callers that need an estimate pay for the extra hub call. The
// deploy path already makes two (validation, then config) and a third on
// every create for a number most deploys never use would be a poor trade.

// treeEntry is one row of huggingface.co/api/models/<id>/tree/<rev>.
//
// Size lives in two places and the outer one lies for large files. A file
// stored through git-lfs reports the pointer's size at the top level (a
// couple of hundred bytes) and the real size under lfs, so reading the outer
// field alone would size a 474 GB checkpoint at about 20 kB. Every weight
// file is lfs-backed by construction, which is exactly the set this cares
// about, so the fallback is for the small files and never for the ones that
// matter.
type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		Size int64 `json:"size"`
	} `json:"lfs"`
}

func (e treeEntry) size() int64 {
	if e.LFS != nil && e.LFS.Size > 0 {
		return e.LFS.Size
	}
	return e.Size
}

// Checkpoint reports what an engine will actually download for this model.
//
// Counts safetensors only, because that is what the engine fetches: vLLM
// derives allow_patterns and takes them alone. A repository that publishes
// the same weights in several formats therefore reports far less here than
// its tree totals, which is the correct answer rather than a conservative
// one. See ModelCheckpoint in the proto for the numbers that make the
// difference concrete.
//
// Returns an error rather than a zero checkpoint when the hub cannot be read.
// Zero bytes is a claim about an empty repository, and a caller estimating a
// download would read it as "already finished".
//
// A repository with no safetensors at all is not an error. It is a model
// published in some other format, and reporting zero files with zero bytes
// says exactly that; the caller decides whether it can work with it.
func (s *Store) Checkpoint(ctx context.Context, req *provisionerv1.DescribeModelRequest) (*provisionerv1.ModelCheckpoint, error) {
	id, revision, err := splitModelSpec(req.GetModelSpec())
	if err != nil {
		return nil, err
	}
	entries, err := s.fetchTree(ctx, id, revision)
	if err != nil {
		return nil, err
	}

	out := &provisionerv1.ModelCheckpoint{}
	for _, e := range entries {
		if e.Type != "file" || !strings.HasSuffix(e.Path, ".safetensors") {
			continue
		}
		n := e.size()
		out.DownloadBytes += n
		out.FileCount++
		if n > out.LargestFileBytes {
			out.LargestFileBytes = n
		}
	}
	return out, nil
}

// fetchTree lists a revision's files, following the hub's pagination.
//
// recursive=true is required: without it the listing stops at the top level
// and a repository keeping its shards in a subdirectory reports nothing,
// which would read as a model with no weights rather than as an unread
// listing.
//
// The Link header carries the next page and the hub does paginate large
// repositories, so a single unpaged request silently truncates the count on
// exactly the checkpoints big enough for any of this to matter.
func (s *Store) fetchTree(ctx context.Context, id, revision string) ([]treeEntry, error) {
	url := strings.TrimRight(s.BaseURL, "/") + "/api/models/" + id + "/tree/" + revision + "?recursive=true"

	var all []treeEntry
	for page := 0; url != "" && page < maxTreePages; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build HF tree request: %w", err)
		}
		if s.Token != "" {
			req.Header.Set("Authorization", "Bearer "+s.Token)
		}
		resp, err := s.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HF API unreachable: %w", err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read HF tree response: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HF tree listing for %q returned %d %s: %s",
				id, resp.StatusCode, http.StatusText(resp.StatusCode), snippet(body))
		}
		var page []treeEntry
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("HF returned 200 but the tree listing was unparseable: %w (body: %s)", err, snippet(body))
		}
		all = append(all, page...)
		url = nextPageURL(resp.Header.Get("Link"))
	}
	return all, nil
}

// maxTreePages bounds the pagination walk. A repository needing more pages
// than this is one nobody is deploying, and an unbounded loop over a header
// the server controls is not something to leave in a deploy path.
const maxTreePages = 50

// nextPageURL pulls the rel="next" target out of an RFC 8288 Link header,
// or "" when there is no next page.
func nextPageURL(header string) string {
	for _, part := range strings.Split(header, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		target := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		for _, p := range segs[1:] {
			if strings.EqualFold(strings.TrimSpace(p), `rel="next"`) {
				return target
			}
		}
	}
	return ""
}
