# SmokePing → gosmokeping config converter

## Problem

Operators migrating from SmokePing to gosmokeping have to translate their
existing config by hand. SmokePing uses a Perl `Config::Grammar` dialect
(sections, `+`/`++`/`+++` depth markers, inheritance, `@include`); gosmokeping
uses flat JSON with named probes and a two-level `group → target` layout. The
two shapes do not line up 1:1, but most of the translation is mechanical and a
one-shot tool can do 95% of the work.

## Goal

- A standalone binary `smokeping2gosmokeping` reads a SmokePing config and
  writes an equivalent gosmokeping JSON config.
- Output is deterministic (identical input produces byte-identical output) so
  re-running on an updated SmokePing config produces a clean diff.
- Best-effort: unmappable probes / alerts / sections are reported to a sidecar
  notes file and the human reviews before production use.
- A `-strict` mode for CI: any partial translation returns exit code 2 so a
  pipeline can gate on a clean conversion.
- Shipped as a second release artifact from the existing GitHub workflow.

## Non-goals

- Not migrating RRD data to InfluxDB — targets / probes / alerts only.
- Not translating SmokePing slaves → gosmokeping cluster slaves. The two
  concepts aren't analogous; SmokePing slaves are data-plane workers reporting
  to a master RRD, gosmokeping slaves speak a different protocol entirely.
- Not supporting every SmokePing probe type. ~20 exist; the tool handles the
  five that cover the common case (`FPing`, `DNS`, `TCPPing`, `Curl`,
  `EchoPingHttp(s)`) and notes the rest.
- Not round-trippable. gosmokeping → SmokePing is not a goal.
- Not trying to preserve SmokePing's `*** Presentation ***` / `*** General ***`
  settings — gosmokeping has no analogue for HTML templates, email-from
  headers, image cache paths, etc.

## Binary & CLI

New binary at `cmd/smokeping2gosmokeping/` in the existing module. Flags:

```
smokeping2gosmokeping \
  -in   <path>   # required, SmokePing config file
  -out  <path>   # required, gosmokeping JSON output path
  -force         # overwrite -out if it already exists (otherwise: error)
  -strict        # exit 2 if any construct could not be fully translated
  -notes <path>  # override notes sidecar path (default: <out>.notes.txt)
  -log-level info|warn|debug  # stderr only; default info
```

Exit codes:
- `0` — success, no partial translations (or partial translations in non-strict
  mode).
- `1` — unrecoverable error: parse failure, IO error, `-out` exists without
  `-force`, include cycle detected, etc.
- `2` — `-strict` mode and at least one partial translation was recorded.

Defaults are CI-friendly: stdout is silent on success, all diagnostics go to
stderr, and the JSON output has a trailing newline and stable key ordering so
it diffs cleanly.

## Architecture

Two-pass: parse into a typed IR, then map IR → `config.Config`. Pure functions
at every layer; no network, no disk beyond the two file reads and two file
writes the CLI driver does.

```
smokeping config → parser → SPRoot (IR) → mapper → *config.Config → json.Marshal → out.json
                                                ↘ notes → out.json.notes.txt
```

Package layout:

```
cmd/smokeping2gosmokeping/
    main.go                      # flag parsing, IO, exit codes
internal/smokepingconv/
    convert.go                   # Convert(in io.Reader, baseDir string) (*config.Config, []Note, error)
    notes.go                     # Note struct, Level/Category, Format()
    parser/
        parser.go                # tokenize + include expansion → []Line
        ir.go                    # IR types (SPRoot, SPProbe, SPAlert, SPNode, SPUnknown)
        build.go                 # []Line → *SPRoot
        parser_test.go
    mapper/
        mapper.go                # top-level: SPRoot → *config.Config
        mapper_database.go
        mapper_probes.go
        mapper_targets.go
        mapper_alerts.go
        mapper_test.go           # + per-file _test.go
    testdata/
        minimal.conf             # golden fixtures
        minimal.want.json
        minimal.want.notes.txt
        ...
```

The `internal/smokepingconv` split is so the CLI stays thin (just flag parsing
+ IO) and the translation logic is unit-testable without shelling out.

## Parser & IR

SmokePing's config format (sans the parts gosmokeping doesn't care about):

- Line-oriented. `#` starts a comment. Blank lines are structure-significant
  (separate nodes under `*** Targets ***`).
- `*** SectionName ***` at column 0 switches section.
- Inside a section, `key = value` lines assign config.
- Under `*** Targets ***` and `*** Probes ***`, `+name` / `++name` / `+++name`
  at column 0 marks a new child node at depth 1 / 2 / 3 / etc. The name is
  used as a URL slug (and group/target identifier).
- A trailing `\` on a value line continues onto the next line (multi-line
  values, e.g. HTTP bodies).
- `@include /path/to/other.conf` pulls in another file inline.

**Stage 1 — tokenizer with include expansion.** Entry point:

```go
package parser

type Line struct {
    Kind     LineKind // Section | Node | Assign | Blank | Comment
    Depth    int      // for Node lines: 1,2,3,...; else 0
    Name     string   // Node name or Assign key; empty for others
    Value    string   // Assign value (multi-line joined); else empty
    Section  string   // current *** Section *** name, propagated onto every Line
    File     string   // absolute path of the file this line came from
    LineNo   int      // 1-based within File
}

func Tokenize(r io.Reader, baseDir, path string) ([]Line, error)
```

`@include` is expanded by recursive call — the included file's lines are
inlined at the include site, with their `File` / `LineNo` pointing at the real
source. Include paths are resolved relative to the including file's directory.
A visited-set keyed on `filepath.Abs(path)` detects cycles; a cycle is a fatal
error (exit code 1).

Multi-line value joins happen here: a line ending in `\` has the `\` stripped
and the next line's content appended (after trimming leading whitespace).

**Stage 2 — build IR.** Walks the `[]Line` and produces:

```go
type SPRoot struct {
    Database  map[string]string   // step, pings, etc. (raw strings; parsed later)
    Probes    []SPProbe           // ordered
    Alerts    []SPAlert           // ordered
    Targets   *SPNode             // root of the target tree (depth 0; no host)
    Unknown   []SPUnknown         // General, Presentation, Slaves, etc.
}

type SPProbe struct {
    Name      string              // e.g. "FPing", or "DNSv4" for a subprobe
    Type      string              // the first * word, e.g. "FPing"; subprobes inherit parent type
    Params    map[string]string   // ordered via Keys slice; raw strings
    Keys      []string            // insertion order, for deterministic emit
    Subprobes []SPProbe           // ordered
    File      string
    LineNo    int
}

type SPAlert struct {
    Name   string
    Params map[string]string
    Keys   []string
    File   string
    LineNo int
}

type SPNode struct {
    Name     string              // identifier from the +name line
    Params   map[string]string   // host, probe, menu, title, alerts, urlformat, ...
    Keys     []string
    Children []*SPNode
    File     string
    LineNo   int
}

type SPUnknown struct {
    Section string
    Lines   []string            // raw text, for the notes file
}
```

`Params` is a map for lookup convenience, but every struct also carries a
`Keys` slice so the mapper can iterate in source order when that matters
(currently only for reporting).

The parser *does not interpret* values (durations, booleans, thresholds stay
as raw strings). Interpretation is the mapper's responsibility — keeps the
parser a small, totally-mechanical stage.

### Parser tests

Golden fixtures in `internal/smokepingconv/testdata/parser/`:

- `minimal.conf` — one `*** Targets ***` with one target.
- `nested.conf` — `+` / `++` / `+++` three-deep.
- `include.conf` + `include_child.conf` — include chain.
- `multiline.conf` — trailing-`\` continuations in `urlformat`.
- `probes.conf` — `+ FPing` with a `++ FPingHighPings` subprobe.

Each has a matching `.ir.json` the test compares against (IR is `json.Marshal`-
round-trippable for test purposes).

## Mapper

`mapper.Convert(root *SPRoot) (*config.Config, []Note)`. Pure; no IO.

### Database section

`*** Database ***` supplies `step` (SmokePing: poll interval in seconds) and
`pings` (samples per poll). Mapping:

- `step N` → `cfg.Interval = Ns` (parsed via `time.ParseDuration`).
- `pings N` → `cfg.Pings = N`.
- Anything else (`min_interval`, `pings_in_graph`, `concurrentprobes`) → note
  (`warn general: database.X ignored`).

Defaults when absent: `Interval = 5m`, `Pings = 20` (matches
`internal/config/config.go` defaults).

### Probes section

Each entry under `*** Probes ***` is either a top-level probe (`+ FPing`) or a
subprobe (`++ FPingHighPings`). Subprobes inherit the parent's type but can
override params.

Type mapping:

| SmokePing type                      | gosmokeping type | Mapped params                         | Unmapped → note    |
|-------------------------------------|------------------|---------------------------------------|--------------------|
| `FPing`, `FPing6`                   | `icmp`           | `timeout` → `timeout`                 | `binary`, `offset`, `packetsize`, `tos` |
| `DNS`                               | `dns`            | `timeout` → `timeout`                 | `server`, `lookup`, `recordtype` (noted as "not yet supported by gosmokeping dns probe") |
| `TCPPing`                           | `tcp`            | `timeout` → `timeout`                 | `port` is applied at target-mapping time (see targets) |
| `Curl`, `EchoPingHttp`, `EchoPingHttps` | `http`       | `timeout` → `timeout`; `insecure_ssl=yes` → `insecure: true` | `urlformat` used at target-mapping time; `useragent`, `follow_redirects`, etc. noted |
| everything else                     | *skip*           | —                                     | `skip probe: <Name> type=<Type>`; targets using this probe are dropped |

Resulting `cfg.Probes` key is the **SmokePing probe name lowercased and
slug-safe**: `FPing` → `fping`, `FPingHighPings` → `fpinghighpings`,
`FPing6` → `fping6`. If after slugging two probes collide, suffix with `-2`,
`-3`, … and emit a warn note.

Each target's `probe = FPing` in SmokePing becomes `"probe": "fping"` in the
gosmokeping target (reference by slugged probe name, not by type). If a target
references a probe name that was never defined in `*** Probes ***` (rare but
legal in SmokePing if the probe uses built-in defaults), a default probe
entry is synthesised under that slugged name — e.g. `probe = FPing` with no
`+ FPing` block yields `probes.fping = { type: "icmp", timeout: "5s" }`.

### Targets section

This is where the structure mismatch lives. gosmokeping has exactly two levels
(group → target). SmokePing allows arbitrary nesting.

**Algorithm:**

1. Walk the target tree depth-first.
2. A node is a *leaf* (becomes a `config.Target`) iff it has `host =` set.
   Intermediate nodes (no host) are *group-path* nodes.
3. For each leaf, compute its **group path**: the slugged names of every
   ancestor between the root and the leaf's parent, joined with `/`. For
   example a leaf at `Targets/Europe/Germany/berlin` produces
   `Group: "europe/germany"`, `Name: "berlin"`. A leaf directly under
   `*** Targets ***` (no intermediate parent) goes to `Group: "default"` to
   satisfy gosmokeping's requirement that every target have a group.
4. Resolve inheritance: walk the ancestor chain from leaf to root; the first
   `probe =` encountered wins (ditto `alerts =`). This matches SmokePing
   semantics where a parent's `probe =` applies to all descendants.
5. `title` / `menu` on a group: pick the leaf's direct parent's `title`
   (falling back to `menu`, then to the slugged group name). A group emits
   once with the `title` of whichever node had the deepest matching title.

**Host / URL construction:**

- For ICMP / TCP / DNS probes: `Host: <host value>`. If the mapped probe is
  `tcp` and the probe def supplied a `port`, append `:port`.
- For HTTP probes: use the probe's `urlformat` (or a default
  `https://%host%/`) with `%host%` substituted. If the result isn't a valid
  URL, drop the target + note.
- If neither `host` nor `url` can be resolved, drop the target + note
  (`warn target: <group>/<name> dropped — probe=X but no host/url`).
- SmokePing doesn't have an `address family` field per target; `Family` is
  always left empty. Noted once at the top of the notes file.

**Group-level dedup:** two leaves that resolve to the same `(group, name)`
produce a name collision. Suffix `-2`, `-3`, … and emit a warn note.

### Alerts section

SmokePing alerts use a pattern DSL: a comma-separated list of per-cycle
matchers evaluated over a sliding window. Examples:

```
+someloss
type = loss
pattern = >0%,>0%,>0%,>0%         # loss on each of the last 4 cycles
+bigloss
type = loss
pattern = ==U                      # last cycle was unreachable
+rttrising
type = rtt
pattern = <10,<10,<10,<10,>100,>100,>100  # went from <10ms to >100ms
```

gosmokeping alerts are a single expression (`loss_pct > 5`) plus a `sustained`
counter. Only a subset of SmokePing patterns fit.

**Heuristics (exhaustive table — anything not listed goes to notes):**

| SmokePing (`type`, `pattern`)                       | gosmokeping                                                      |
|-----------------------------------------------------|------------------------------------------------------------------|
| `loss`, `>X%,>X%,…,>X%` (N consecutive, same X)     | `condition: "loss_pct > X"`, `sustained: N`                      |
| `rtt`,  `>X,>X,…,>X` (N consecutive, same X; assume ms) | `condition: "rtt_median > Xms"`, `sustained: N`              |
| `loss`, `==U` or `==100%` (single matcher)          | `condition: "loss_pct >= 100"`, `sustained: 1`                   |
| `loss`, `==U,==U,…` (N Us)                          | `condition: "loss_pct >= 100"`, `sustained: N`                   |
| anything else                                       | skip; note records the original pattern verbatim                 |

`to = …` (email destinations) → note. gosmokeping has no native email action
yet. The alert still emits with `actions: ["log"]` so it's wired up; operator
adds a webhook / discord action in the gosmokeping config after migration.

Alerts that land in `cfg.Alerts` get an entry of `actions: ["log"]` and
`cfg.Actions["log"] = {type: "log"}` is always emitted, so the generated
config validates out of the box.

### Top-level emit

```go
cfg := &config.Config{
    Listen:   ":8080",
    Interval: <from Database or default 5m>,
    Pings:    <from Database or default 20>,
    Storage: config.Storage{
        Backend: config.BackendInfluxV2,
        InfluxV2: config.InfluxV2{
            URL:       "${INFLUX_URL}",
            Token:     "${INFLUX_TOKEN}",
            Org:       "${INFLUX_ORG}",
            BucketRaw: "smokeping_raw",
            Bucket1h:  "smokeping_1h",
            Bucket1d:  "smokeping_1d",
        },
    },
    Probes:  <from Probes section + synthesised defaults>,
    Targets: <flattened groups>,
    Alerts:  <translated alerts>,
    Actions: map[string]config.Action{"log": {Type: "log"}},
}
```

A note always reminds the operator: "storage.influxv2 is a placeholder — edit
URL/token/org before running gosmokeping."

## Output & notes sidecar

### JSON output

Produced via `json.MarshalIndent(cfg, "", "  ")` with a trailing `\n`. Map
iteration is normalized for determinism:

- `probes` — emitted in a fixed order: `icmp`, `tcp`, `http`, `dns`, then any
  extras sorted alphabetically by key.
- `alerts` and `actions` — sorted alphabetically by key.
- `targets` (slice) — preserves SmokePing definition order; within each group,
  target order preserves leaf discovery order from the tree walk.
- `storage` / `cluster` / `listen` / `interval` / `pings` — natural struct
  field order from `config.Config` (Go's JSON encoder is deterministic for
  structs).

Because `config.Config` defines `probes` / `alerts` / `actions` as `map[string]…`,
Go's default JSON encoder would sort map keys alphabetically. To get the fixed
probe ordering, we marshal via a shim struct that uses `json.RawMessage` for
those fields and builds them with the desired ordering.

Alternative considered: skip the custom ordering and let Go's default
alphabetical sort run for maps. That's simpler and still deterministic, just
not as human-friendly. **Decision:** do the fixed probe order (icmp first) —
it's ~20 lines and matches `config.example.json`.

### Notes sidecar

Format per line:

```
<level> <category>: <detail>  (source: <file>:<lineno>)
```

Levels: `skip` (dropped entirely) or `warn` (translated with loss of info).
Categories: `probe`, `target`, `alert`, `general`, `include`, `section`.

Examples:

```
skip probe: FPing6 "fping6-jitter" — no gosmokeping equivalent  (source: /etc/smokeping/config:58)
warn alert: pattern ">0%,*12*,>0%,*12*,>0%" simplified to loss_pct > 0 sustained=3 — verify  (source: /etc/smokeping/alerts.conf:14)
warn target: Europe/Germany/Berlin probe=Curl has no urlformat — dropped  (source: /etc/smokeping/targets.conf:204)
skip section: *** Presentation *** ignored (no gosmokeping analogue)
warn general: storage.influxv2 is a placeholder — edit before running
```

Emit rules:
- If the notes slice is empty, no sidecar file is written. Keeps CI output
  quiet when the conversion is clean.
- In `-strict` mode, any `skip` or `warn` line triggers exit code 2 *after*
  writing both the JSON and notes file. The operator sees both outputs; CI
  just fails on the nonzero exit.

## Build + GitHub workflow

### Makefile

New targets appended to the existing file:

```makefile
smokeping2gosmokeping:
	$(GO) build -ldflags="$(LDFLAGS)" -o smokeping2gosmokeping ./cmd/smokeping2gosmokeping

build-all: build smokeping2gosmokeping
```

The existing `build` target is unchanged so normal developer workflow stays
one command.

### GitHub Actions

`.github/workflows/build.yml`, in the `binary` job, after the existing
gosmokeping `Build binary` step:

```yaml
- name: Build smokeping2gosmokeping
  run: go build -trimpath -ldflags="-s -w" -o build/smokeping2gosmokeping-linux-amd64 ./cmd/smokeping2gosmokeping

- uses: actions/upload-artifact@v6
  with:
    name: smokeping2gosmokeping-linux-amd64
    path: build/smokeping2gosmokeping-linux-amd64
```

And in the `Create GitHub release` step, include the new binary in the
`gh release create` file list.

## Testing strategy

1. **Parser unit tests** (`internal/smokepingconv/parser/parser_test.go`):
   golden `.conf` → expected IR, diffed via `go-cmp`. Covers: nested depth
   markers, `@include` chains, include cycles (must error), multi-line
   `\`-continuations, comments, section switching.
2. **Mapper unit tests** (`internal/smokepingconv/mapper/*_test.go`):
   table-driven per mapper file (database, probes, targets, alerts), each
   asserting both the `*config.Config` shape and the set of emitted notes.
3. **End-to-end golden tests** (`internal/smokepingconv/convert_test.go`):
   `testdata/*.conf` → compare against `testdata/*.want.json` and
   `testdata/*.want.notes.txt`. A `go test -update` flag regenerates goldens.
4. **Validation gate**: every generated JSON in the end-to-end tests is passed
   through `config.Load` (using a temp file — Load is file-based) to confirm
   it parses and validates. Prevents the mapper from ever emitting a
   structurally-invalid gosmokeping config even when the input is weird.
5. **No integration tests** — converter is pure stdlib, no network / InfluxDB.

Fixtures to include at minimum:

- `minimal.conf` — one FPing target, one alert.
- `nested.conf` — four levels of `+` nesting + inherited `probe`.
- `mixed_probes.conf` — FPing + DNS + Curl + TCPPing.
- `with_includes.conf` — one `@include`.
- `unsupported.conf` — EchoPingSSH, complex alert patterns, Presentation
  section — exercises the notes file.

## Determinism requirements (recap)

For CI use, identical input must produce byte-identical output. Sources of
non-determinism to suppress:

- Go map iteration — everywhere an `SP*.Params` map is iterated for emit, use
  the `Keys` companion slice.
- `cfg.Probes` / `cfg.Alerts` / `cfg.Actions` encode order — handled via the
  custom-ordering JSON shim described above.
- `filepath.Glob` results — not used; `@include` takes explicit paths.
- Time — converter records no timestamps in its output.

## Rollout

1. Land the new binary + tests + workflow change in one PR.
2. Cut a release (existing tag-triggered workflow publishes both binaries).
3. Document in `README.md` under a new "Migrating from SmokePing" section.
4. No config changes to the main `gosmokeping` binary — this is additive.
