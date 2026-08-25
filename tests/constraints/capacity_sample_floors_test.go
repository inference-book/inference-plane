package constraints

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The sampler has to ask more than one VRAM floor, and one of them has to be
// high enough to reach the top of the catalog.
//
// `iplane capacity` returns the cheapest few SKUs above the floor, capped at
// skucatalog.MaxResults so an operator asking for a small card does not land
// on a frontier one. That cap is right. It also means a single low floor is a
// window rather than a floor, and the sampler was configured with one: six
// days of eight-card observations recorded no Blackwell while 8x B200 was
// live on Vast at $47/hr.
//
// This is asserted here rather than in the script because a failure in that
// log is invisible by construction. The whole point of the series is to tell
// "nobody had any" apart from "nobody looked", and a blind floor produces the
// first while meaning the second.
//
// internal/provisioners/vast/vramfloor_test.go pins the other half: that a
// floor of 80 genuinely cannot reach Blackwell and 140 can, against the real
// catalog, so a catalog change fails there and sends somebody back here.
func TestCapacitySamplerAsksAFloorThatReachesTheTopOfTheMarket(t *testing.T) {
	path := filepath.Join(repoRoot(t), "hack", "capacity-sample.sh")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	m := regexp.MustCompile(`MIN_VRAMS?="\$\{CAPACITY_SAMPLE_MIN_VRAM_GB:-([^}]*)\}"`).FindSubmatch(body)
	if m == nil {
		t.Fatal("could not find the sampler's default VRAM floors; if the variable was renamed, this test needs to follow it")
	}
	fields := strings.Fields(string(m[1]))
	if len(fields) < 2 {
		t.Fatalf("the sampler defaults to %d floor(s): %v. One floor plus the resolver's cap is a window, not a floor, and the top of the market never appears in the log",
			len(fields), fields)
	}

	highest := 0
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("floor %q is not a number", f)
		}
		if n > highest {
			highest = n
		}
	}
	// Above every Hopper part, so the cheapest-five cut cannot be filled by
	// them. H100_NVL is the tallest at 94 GB.
	//
	// The exact figure that reaches Blackwell is catalog knowledge and lives
	// with the catalog: internal/provisioners/vast/vramfloor_test.go asserts
	// that the sampler's high floor resolves B200 and B300 for real. Pinning
	// it in both places would mean a catalog change failing here with no
	// clue about what to change it to.
	if highest <= 94 {
		t.Errorf("the sampler's highest floor is %d GB, which every Hopper part clears, so the cheapest five can be Hopper and no Blackwell appears", highest)
	}
}
