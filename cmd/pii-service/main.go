// Command pii-service is the entry point of the PII protection service.
package main

import (
	"fmt"
	"os"

	"github.com/klrushka/llm-proxy/internal/version"
)

func main() {
	fmt.Fprintf(os.Stdout, "pii-service %s\n", version.Version)
}
