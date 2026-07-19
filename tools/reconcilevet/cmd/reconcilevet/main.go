// Command reconcilevet is a standalone driver for the reconcilevet analyzer.
//
// Install it and run it as a go vet tool:
//
//	go install github.com/xavidop/mamori/tools/reconcilevet/cmd/reconcilevet@latest
//	go vet -vettool=$(which reconcilevet) ./...
package main

import (
	"github.com/xavidop/mamori/tools/reconcilevet"

	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(reconcilevet.Analyzer)
}
