// Command pii-harness runs the local quality harness over the checked-in
// synthetic corpus and prints a machine-readable JSON report. It uses the
// deterministic Go rules/validators predictor (no NER, no network) and reports
// honest per-type precision/recall/F1, normalized span-based Levenshtein
// masking quality and exact-match restoration. It does not claim the official
// checker result or the 95 percent target.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/klrushka/llm-proxy/internal/harness"
	"github.com/klrushka/llm-proxy/internal/testcorpus"
)

func main() {
	corpusPath := flag.String("corpus", "testdata/pii-corpus.json", "path to the synthetic corpus JSON")
	flag.Parse()

	corpus, err := testcorpus.Load(*corpusPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pii-harness: load corpus: %v\n", err)
		os.Exit(1)
	}
	if err := testcorpus.Validate(corpus); err != nil {
		fmt.Fprintf(os.Stderr, "pii-harness: validate corpus: %v\n", err)
		os.Exit(1)
	}

	report, err := harness.Run(context.Background(), corpus, harness.RulesPredictor{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pii-harness: run: %v\n", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "pii-harness: encode report: %v\n", err)
		os.Exit(1)
	}
}
