// Command iplane is the operator and developer CLI for the inference
// control plane. Three subcommands:
//
//	iplane serve       run the control plane server
//	iplane load        fire synthetic traffic for demos / dashboard authoring
//	iplane gen-names   regenerate the OTel name vocabulary (Go consts + book LaTeX)
//
// Subcommand wiring lives in cmd/iplane/cmd/. main is intentionally thin
// so the docker image's ENTRYPOINT can be ["/app/iplane", "serve"] and
// the same binary can be invoked locally for ad-hoc tooling.
package main

import "github.com/inference-book/inference-plane/cmd/iplane/cmd"

func main() {
	cmd.Execute()
}
