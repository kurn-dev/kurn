// Command embed shows Kurn running as a Go library inside an application.
// It uses invented, application-owned data and a temporary store so the
// example leaves no files behind.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/kurn-dev/kurn/engine"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(stdout io.Writer) (err error) {
	dataDir, err := os.MkdirTemp("", "kurn-embed-")
	if err != nil {
		return fmt.Errorf("create temporary data directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(dataDir); removeErr != nil && err == nil {
			err = fmt.Errorf("remove temporary data directory: %w", removeErr)
		}
	}()

	store, err := engine.Open(dataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close store: %w", closeErr)
		}
	}()

	catalog, err := store.CreateList("catalog", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "free-text"},
		Match:    engine.MatchConfig{Mode: "ngram", StripSpaces: true},
		Filterable: []engine.FilterField{
			{Name: "available", Path: "available"},
		},
	})
	if err != nil {
		return fmt.Errorf("create catalog list: %w", err)
	}
	if err := store.Replace("catalog", []engine.Entry{
		{ID: "item-1", Keys: []string{"Harbor Desk Lamp"}, Payload: json.RawMessage(`{"available":true}`)},
		{ID: "item-2", Keys: []string{"Harbor Desk Lamp"}, Payload: json.RawMessage(`{"available":false}`)},
	}); err != nil {
		return fmt.Errorf("replace catalog entries: %w", err)
	}

	available, err := engine.ParseTypedFilter([]byte(`{"available":true}`))
	if err != nil {
		return fmt.Errorf("parse typed filter: %w", err)
	}
	fuzzy, fuzzyVersion, err := catalog.QueryTypedFilteredCtx(
		context.Background(), "harbor desk lmap", engine.QueryOpts{}, available,
	)
	if err != nil {
		return fmt.Errorf("query catalog: %w", err)
	}
	if err := printSingleAnswer(stdout, "fuzzy+filter", fuzzy, fuzzyVersion, "item-1"); err != nil {
		return err
	}

	codes, err := store.CreateList("codes", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "identifier"},
		Match:    engine.MatchConfig{Mode: "exact"},
	})
	if err != nil {
		return fmt.Errorf("create codes list: %w", err)
	}
	if err := store.Replace("codes", []engine.Entry{
		{ID: "code-1", Keys: []string{"SKU-1042"}},
		{ID: "code-2", Keys: []string{"SKU-2048"}},
	}); err != nil {
		return fmt.Errorf("replace code entries: %w", err)
	}
	exact, exactVersion := codes.QueryVersioned(
		context.Background(), " sku-1042 ", engine.QueryOpts{},
	)
	return printSingleAnswer(stdout, "exact", exact, exactVersion, "code-1")
}

func printSingleAnswer(stdout io.Writer, label string, candidates []engine.Candidate, version, wantID string) error {
	if version == "" {
		return fmt.Errorf("%s query returned an empty version", label)
	}
	if len(candidates) != 1 || candidates[0].EntryID != wantID {
		return fmt.Errorf("%s query returned %+v, want only %q", label, candidates, wantID)
	}
	candidate := candidates[0]
	_, err := fmt.Fprintf(stdout, "%s: id=%s score=%.1f version=%s\n", label, candidate.EntryID, candidate.Score, version)
	return err
}
