package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: beats <command> [flags]")
		fmt.Fprintln(os.Stderr, "  beats init    --repo <path> [--dry-run]")
		fmt.Fprintln(os.Stderr, "  beats analyze --repo <path>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		runInit(os.Args[2:])
	case "analyze":
		runAnalyze(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		fmt.Fprintln(os.Stderr, "  beats init    --repo <path> [--dry-run]")
		fmt.Fprintln(os.Stderr, "  beats analyze --repo <path>")
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

func runAnalyze(args []string) {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	prj := fs.String("repo", "", "Path to the repository to analyze")
	_ = fs.Parse(args)

	if *prj == "" {
		fmt.Fprintln(os.Stderr, "beats analyze: --repo is required")
		os.Exit(1)
	}

	if err := RunAnalyze(*prj); err != nil {
		slog.Error("unable to run beats analyze", slog.String("repo", *prj), slog.Any("error", err))
		os.Exit(1)
	}
}
