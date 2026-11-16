package main

import (
	"fmt"
	"github.com/11bronx11/bx_yunpan/backend/internal/bootstrap"
	"os"
)

func main() {
	switch os.Getenv("AI_PROJECTION_ROLE") {
	case "text":
		bootstrap.RunAIRole("ai-text-projector")
	case "vector":
		bootstrap.RunAIRole("ai-vector-projector")
	default:
		fmt.Fprintln(os.Stderr, "search-indexer requires AI_PROJECTION_ROLE=text|vector; use explicit projectors")
		os.Exit(2)
	}
}
