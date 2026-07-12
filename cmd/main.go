package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

// default
const Version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		runInit(os.Args[2:])
	case "analyze":
		runAnalyzeCmd(os.Args[2:])
	case "query":
		runQuery(os.Args[2:])
	case "calibrate":
		runCalibrate(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("beats %s\n", Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: beats <command> [flags]")
	fmt.Fprintln(os.Stderr, "  beats init      --repo <path> [--dry-run]")
	fmt.Fprintln(os.Stderr, "  beats query     outlier              --repo <path> [--format text|json]")
	fmt.Fprintln(os.Stderr, "  beats query     cluster shape <hash> --repo <path> [--format text|json]")
	fmt.Fprintln(os.Stderr, "  beats analyze   --repo <path>")
	fmt.Fprintln(os.Stderr, "  beats calibrate --repo <path> [--bootstraps N]")
	fmt.Fprintln(os.Stderr, "  beats version")
}

// runAnalyzeCmd regenerates the HTML report from whatever is currently in the
// database — including any LLM enrichment written by beats update cluster.
// It is intentionally separate from beats init so the report can be refreshed
// after enrichment without re-indexing the whole repository.
func runAnalyzeCmd(args []string) {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	repo := fs.String("repo", "", "Path to the repository (same value passed to beats init)")
	_ = fs.Parse(args)

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "beats analyze: --repo is required")
		os.Exit(1)
	}

	if err := runAnalyze(*repo); err != nil {
		slog.Error("beats analyze failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	prj := fs.String("repo", "", "Path to the repository to index")
	isDryRun := fs.Bool("dry-run", false, "Execute beats in dry run mode, no persistence to file or db")
	_ = fs.Parse(args)

	if *prj == "" {
		fmt.Fprintln(os.Stderr, "beats init: --repo is required")
		os.Exit(1)
	}

	b := &Beats{IsDryRun: *isDryRun}
	if err := b.run(*prj); err != nil {
		slog.Error("unable to create beats index", slog.String("repo", *prj), slog.Any("error", err))
		os.Exit(1)
	}
	slog.Info("successfully created beats index and cluster", slog.String("repo", *prj))
}
