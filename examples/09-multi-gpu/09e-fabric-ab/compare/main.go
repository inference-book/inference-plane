package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// Thin CLI over compare.go. Reads the JSON summaries `iplane load --output
// json` wrote for each arm and prints the comparison block.
//
// Exit status is deliberately 0 even when no difference is established: "no
// difference" is a legitimate experimental outcome here, not a failure, and a
// non-zero exit would make run.sh look broken when it had in fact worked.
// Non-zero is reserved for "the comparison could not be performed".
func main() {
	var (
		aLabel = flag.String("a-label", "arm-a", "label for arm A")
		bLabel = flag.String("b-label", "arm-b", "label for arm B")
		aFiles = flag.String("a", "", "comma-separated JSON summary files for arm A")
		bFiles = flag.String("b", "", "comma-separated JSON summary files for arm B")
	)
	flag.Parse()

	if *aFiles == "" || *bFiles == "" {
		fmt.Fprintln(os.Stderr, "usage: compare --a-label nvlink --a a1.json,a2.json --b-label pcie --b b1.json,b2.json")
		os.Exit(2)
	}

	a, err := LoadArm(*aLabel, split(*aFiles))
	if err != nil {
		fmt.Fprintf(os.Stderr, "arm A: %v\n", err)
		os.Exit(1)
	}
	b, err := LoadArm(*bLabel, split(*bFiles))
	if err != nil {
		fmt.Fprintf(os.Stderr, "arm B: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(Render(a, b, Compare(a, b), Warnings(a, b)))
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
