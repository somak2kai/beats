package main

import (
	"flag"
	"log/slog"
	log "log/slog"
)

func main() {

	prj := flag.String("repo", "", "Provide repository path")
	isDryRun := flag.Bool("dry-run", false, "Execute beats in dry run mode, no persistence to file or db")
	flag.Parse()

	b := &Beats{IsDryRun: *isDryRun}
	if err := b.run(*prj); err != nil {
		log.Error("unable to create beats index", slog.String("repo", *prj), slog.Any("error", err))
		return
	}
	log.Info("successfully created beats index and cluster")
}
