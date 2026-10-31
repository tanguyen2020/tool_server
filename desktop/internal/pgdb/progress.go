package pgdb

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ------------------------------------------------------------------ table progress

// Active is a table being copied right now.
type Active struct {
	Name  string `json:"name"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

type item struct {
	key       string // schema.name
	weight    int64  // share of the phase total (table size, or file size on a restore)
	ref       int64  // COPY bytes_processed is compared to this to estimate a running table
	state     int    // 0 waiting, 1 running, 2 done
	processed int64
	polled    bool // seen in pg_stat_progress_copy
}

// tracker follows the tables of a dump or a restore. The verbose output of pg_dump / pg_restore says when a
// table starts and finishes; pg_stat_progress_copy (PostgreSQL 14+) says how far a running table is.
type tracker struct {
	items  map[string]*item
	byName map[string][]*item // table name without schema -> items (pg_dump's "finished item" only gives the name)
	total  int64
	nDone  int
}

func newTracker() *tracker {
	return &tracker{items: map[string]*item{}, byName: map[string][]*item{}}
}

func (t *tracker) add(key string, weight, ref int64) {
	if weight <= 0 {
		weight = 1 // empty tables still count, a little
	}
	if ref <= 0 {
		ref = weight
	}
	if it, ok := t.items[key]; ok {
		t.total += weight - it.weight
		it.weight, it.ref = weight, ref
		return
	}
	it := &item{key: key, weight: weight, ref: ref}
	t.items[key] = it
	_, name, _ := strings.Cut(key, ".")
	t.byName[name] = append(t.byName[name], it)
	t.total += weight
}

func (t *tracker) start(key string) {
	if it := t.items[key]; it != nil && it.state == 0 {
		it.state = 1
	}
}

func (t *tracker) finish(key string) {
	if it := t.items[key]; it != nil {
		t.finishItem(it)
	}
}

func (t *tracker) finishItem(it *item) {
	if it.state != 2 {
		it.state = 2
		t.nDone++
	}
}

// finishName marks a table done by its name alone, preferring one that is running.
func (t *tracker) finishName(name string) {
	var pick *item
	for _, it := range t.byName[name] {
		if it.state == 1 {
			pick = it
			break
		}
		if it.state == 0 && pick == nil {
			pick = it
		}
	}
	if pick != nil {
		t.finishItem(pick)
	}
}

// poll applies one reading of pg_stat_progress_copy: key -> bytes processed. A table that was running in the
// previous reading and is gone now has finished.
func (t *tracker) poll(active map[string]int64) {
	for _, it := range t.items {
		if b, ok := active[it.key]; ok {
			if it.state == 0 {
				it.state = 1
			}
			it.processed, it.polled = b, true
		} else if it.polled && it.state == 1 {
			t.finishItem(it)
		}
	}
}

// finishAll is called when the tool exited successfully.
func (t *tracker) finishAll() {
	for _, it := range t.items {
		t.finishItem(it)
	}
}

// progress returns the weight done (running tables count by their COPY progress) and the running tables.
func (t *tracker) progress() (done int64, active []Active) {
	for _, it := range t.items {
		switch it.state {
		case 2:
			done += it.weight
		case 1:
			frac := 0.0
			if it.ref > 0 {
				frac = float64(it.processed) / float64(it.ref)
			}
			if frac > 0.97 {
				frac = 0.97
			}
			part := int64(frac * float64(it.weight))
			done += part
			active = append(active, Active{Name: it.key, Done: part, Total: it.weight})
		}
	}
	sort.Slice(active, func(a, b int) bool { return active[a].Total > active[b].Total })
	if len(active) > 12 {
		active = active[:12]
	}
	return done, active
}

// ------------------------------------------------------------------ verbose output

var (
	dumpTableRe   = regexp.MustCompile(`^pg_dump: dumping contents of table "(.+)"$`)
	finishedRe    = regexp.MustCompile(`^pg_(?:dump|restore): finished item (\d+) (.+)$`)
	launchRe      = regexp.MustCompile(`^pg_restore: (?:launching|processing) item (\d+) `)
	restoreDataRe = regexp.MustCompile(`^pg_restore: processing data for table "(.+)"$`)
	creatingRe    = regexp.MustCompile(`^pg_restore: creating (INDEX|CONSTRAINT|FK CONSTRAINT|CHECK CONSTRAINT|TRIGGER|RULE|POLICY|INDEX ATTACH|STATISTICS|EVENT TRIGGER) `)
	ignoredRe     = regexp.MustCompile(`errors ignored on restore: (\d+)`)
)

// ------------------------------------------------------------------ TOC (pg_restore -l)

// tocEntry is one line of `pg_restore -l`.
type tocEntry struct {
	ID   int
	Kind string // data, post (indexes, constraints, triggers… built after the data) or pre
	Key  string // schema.name for table data
}

var tocRe = regexp.MustCompile(`^(\d+); \d+ \d+ (.+)$`)

// Created after the data: indexes, constraints, triggers and the like.
var postDescs = []string{"INDEX ATTACH ", "INDEX ", "FK CONSTRAINT ", "CHECK CONSTRAINT ", "CONSTRAINT ", "EVENT TRIGGER ", "TRIGGER ",
	"RULE ", "POLICY ", "MATERIALIZED VIEW DATA ", "STATISTICS ", "PUBLICATION TABLE "}

func parseTOC(out string) []tocEntry {
	var list []tocEntry
	for _, line := range strings.Split(out, "\n") {
		m := tocRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		id, _ := strconv.Atoi(m[1])
		e := tocEntry{ID: id, Kind: "pre"}
		rest := m[2]
		if strings.HasPrefix(rest, "TABLE DATA ") {
			e.Kind = "data"
			// "<schema> <table> <owner>": the table name may contain spaces, the schema and owner rarely do.
			f := strings.Fields(strings.TrimPrefix(rest, "TABLE DATA "))
			if len(f) >= 3 {
				e.Key = f[0] + "." + strings.Join(f[1:len(f)-1], " ")
			} else if len(f) == 2 {
				e.Key = f[0] + "." + f[1]
			}
		} else {
			for _, d := range postDescs {
				if strings.HasPrefix(rest, d) {
					e.Kind = "post"
					break
				}
			}
		}
		list = append(list, e)
	}
	return list
}

// parseLs reads `ls -ln` of the dump folder: file name -> size.
func parseLs(out string) map[string]int64 {
	files := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 9 || strings.HasPrefix(f[0], "d") {
			continue
		}
		size, err := strconv.ParseInt(f[4], 10, 64)
		if err != nil {
			continue
		}
		files[strings.Join(f[8:], " ")] = size
	}
	return files
}

// dataFileSizes maps a TOC id to the size of its data file ("1234.dat.zst" -> 1234).
func dataFileSizes(files map[string]int64) map[int]int64 {
	sizes := map[int]int64{}
	for name, size := range files {
		num, rest, ok := strings.Cut(name, ".")
		if !ok || !strings.HasPrefix(rest, "dat") {
			continue
		}
		if id, err := strconv.Atoi(num); err == nil {
			sizes[id] = size
		}
	}
	return sizes
}

// ------------------------------------------------------------------ pg_stat_progress_copy

// progressSQL lists the COPY commands running in the current database (PostgreSQL 14+).
const progressSQL = `select n.nspname, c.relname, p.bytes_processed from pg_stat_progress_copy p
join pg_class c on c.oid = p.relid join pg_namespace n on n.oid = c.relnamespace
where p.datname = current_database() and p.command = '%s';`

func parseProgress(out string) map[string]int64 {
	active := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) != 3 {
			continue
		}
		b, err := strconv.ParseInt(f[2], 10, 64)
		if err == nil {
			active[f[0]+"."+f[1]] += b
		}
	}
	return active
}
