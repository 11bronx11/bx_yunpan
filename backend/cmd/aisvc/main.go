package main

import (
	"fmt"
	"github.com/11bronx11/bx_yunpan/backend/internal/bootstrap"
	"os"
)

func main() {
	role := os.Getenv("AI_SERVICE_ROLE")
	if role == "query" {
		bootstrap.RunAIRole("ai-query")
		return
	}
	if role == "indexer" {
		bootstrap.RunAIRole("ai-embedding-worker")
		return
	}
	fmt.Fprintln(os.Stderr, "aisvc combined has retired; use ai-query and explicit AI compute/committer roles")
	os.Exit(2)
}
