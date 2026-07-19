package reconcilevet

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestReconcilevet(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "a")
}
