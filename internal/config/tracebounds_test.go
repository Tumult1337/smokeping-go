package config_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/tumult/gosmokeping/internal/config"
)

// config.MaxTraceRounds / MaxTraceTTL mirror literals that live in
// internal/probe, and cluster's ingest bound is derived from them. probe
// cannot import config's mirror back for a compile-time assertion the way
// icmpTraceSeqReserve is pinned (probe already imports config, and the values
// are unexported), so the pin is this test: it reads probe's own source and
// fails naming the value to update.
func TestTraceBoundsMirrorProbe(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../probe/mtr.go", nil, 0)
	if err != nil {
		t.Fatalf("parse probe/mtr.go: %v", err)
	}

	rounds, ttl := -1, -1
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			if len(v.Names) == 1 && v.Names[0].Name == "maxRounds" && len(v.Values) == 1 {
				rounds = intLit(t, v.Values[0])
			}
		case *ast.KeyValueExpr:
			if key, ok := v.Key.(*ast.Ident); ok && key.Name == "maxTTL" {
				ttl = intLit(t, v.Value)
			}
		}
		return true
	})

	if rounds < 0 || ttl < 0 {
		t.Fatalf("could not find maxRounds/maxTTL in probe/mtr.go (found %d/%d) — the mirror in config can no longer be checked", rounds, ttl)
	}
	if rounds != config.MaxTraceRounds {
		t.Errorf("probe maxRounds = %d, config.MaxTraceRounds = %d: update the mirror and cluster.MaxHopsPerCycle follows", rounds, config.MaxTraceRounds)
	}
	if ttl != config.MaxTraceTTL {
		t.Errorf("probe MTR.maxTTL = %d, config.MaxTraceTTL = %d: update the mirror and cluster.MaxHopsPerCycle follows", ttl, config.MaxTraceTTL)
	}
}

func intLit(t *testing.T, e ast.Expr) int {
	t.Helper()
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		t.Fatalf("expected an int literal, got %T", e)
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil {
		t.Fatalf("parse %q: %v", lit.Value, err)
	}
	return n
}
