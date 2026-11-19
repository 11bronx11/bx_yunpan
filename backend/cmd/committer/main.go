package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "committer is retired; domain workers compute and commit with coordination protocol v2")
	os.Exit(1)
}
