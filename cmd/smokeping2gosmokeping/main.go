package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/smokepingconv"
)

func main() {
	var (
		inPath    = flag.String("in", "", "path to SmokePing config (required)")
		outPath   = flag.String("out", "", "path to gosmokeping JSON output (required)")
		notesPath = flag.String("notes", "", "notes sidecar path (default: <out>.notes.txt)")
		force     = flag.Bool("force", false, "overwrite -out if it exists")
		strict    = flag.Bool("strict", false, "exit 2 if any construct could not be fully translated")
		logLevel  = flag.String("log-level", "info", "log level: debug|info|warn|error")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)})))

	if *inPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "error: -in and -out are required")
		flag.Usage()
		os.Exit(1)
	}
	if *notesPath == "" {
		*notesPath = *outPath + ".notes.txt"
	}

	if err := run(*inPath, *outPath, *notesPath, *force, *strict); err != nil {
		var strictErr *strictError
		if ok := asStrict(err, &strictErr); ok {
			slog.Warn(strictErr.Error())
			os.Exit(2)
		}
		slog.Error(err.Error())
		os.Exit(1)
	}
}

type strictError struct{ n int }

func (e *strictError) Error() string {
	return fmt.Sprintf("strict mode: %d note(s) produced", e.n)
}

func asStrict(err error, target **strictError) bool {
	s, ok := err.(*strictError)
	if ok {
		*target = s
	}
	return ok
}

func run(inPath, outPath, notesPath string, force, strict bool) error {
	if !force {
		if _, err := os.Stat(outPath); err == nil {
			return fmt.Errorf("output %s already exists (use -force to overwrite)", outPath)
		}
	}
	f, err := os.Open(inPath)
	if err != nil {
		return fmt.Errorf("open -in: %w", err)
	}
	defer f.Close()

	absIn, _ := filepath.Abs(inPath)
	cfg, notes, err := smokepingconv.Convert(f, filepath.Dir(absIn), absIn)
	if err != nil {
		return err
	}

	// Validate the emitted config — catches mapper bugs before we write.
	if err := cfg.Validate(); err != nil {
		slog.Warn("emitted config fails validation — writing anyway for operator review", "err", err)
	}

	data, err := smokepingconv.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := writeFile(outPath, data); err != nil {
		return fmt.Errorf("write -out: %w", err)
	}

	if len(notes) > 0 {
		if err := writeNotes(notesPath, notes); err != nil {
			return fmt.Errorf("write notes: %w", err)
		}
		for _, n := range notes {
			slog.Info(n.Format())
		}
	}

	if strict && len(notes) > 0 {
		return &strictError{n: len(notes)}
	}
	return nil
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeNotes(path string, notes []smokepingconv.Note) error {
	// Sort for determinism: level first (skip > warn), then category, then detail.
	sorted := append([]smokepingconv.Note(nil), notes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Level != sorted[j].Level {
			return sorted[i].Level < sorted[j].Level
		}
		if sorted[i].Category != sorted[j].Category {
			return sorted[i].Category < sorted[j].Category
		}
		return sorted[i].Detail < sorted[j].Detail
	})
	var b strings.Builder
	for _, n := range sorted {
		b.WriteString(n.Format())
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

var _ = config.BackendInfluxV2 // ensure import is not dropped if Validate() stays commented
