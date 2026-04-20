package smokepingconv

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

const (
	defaultInterval = 5 * time.Minute
	defaultPings    = 20
)

// mapDatabase translates the *** Database *** section into interval + pings.
// Unknown keys produce warn notes so the operator knows they were seen and
// deliberately dropped.
func mapDatabase(root *parser.SPRoot) (time.Duration, int, []Note) {
	var notes []Note
	interval := defaultInterval
	pings := defaultPings

	if v, ok := root.Database["step"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			notes = append(notes, Note{
				Level: LevelWarn, Category: CatGeneral,
				Detail: fmt.Sprintf("database.step %q invalid — keeping default %s", v, defaultInterval),
			})
		} else {
			interval = time.Duration(n) * time.Second
		}
	}
	if v, ok := root.Database["pings"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			notes = append(notes, Note{
				Level: LevelWarn, Category: CatGeneral,
				Detail: fmt.Sprintf("database.pings %q invalid — keeping default %d", v, defaultPings),
			})
		} else {
			pings = n
		}
	}

	var extras []string
	for k := range root.Database {
		if k == "step" || k == "pings" {
			continue
		}
		extras = append(extras, k)
	}
	sort.Strings(extras)
	for _, k := range extras {
		notes = append(notes, Note{
			Level: LevelWarn, Category: CatGeneral,
			Detail: fmt.Sprintf("database.%s ignored (no gosmokeping analogue)", k),
		})
	}

	return interval, pings, notes
}
