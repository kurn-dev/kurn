// Package ingest turns external list feeds (NDJSON, CSV, streaming XML)
// into engine entries through a declarative mapping. The
// mapping is deliberately small: dot-paths and equals-only filters, the
// minimum that maps real-world feeds (OFAC SDN is the proof corpus);
// anything needing more is custom code by design, not a bigger language.
//
// Parsers are stdlib-only and streaming: a 10 GB input never resides in
// memory — records are decoded one at a time and yielded as they parse.
// The claim is enforced, not assumed: every format bounds one record, and
// an oversize one is a bad record the run can skip (SkipBad) rather than
// something that ends the run or, worse, materializes. The single
// exception is a CSV record that never terminates — there is no next
// record to resume at, so it is fatal at a ceiling well above the record
// bound.
package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kurn-dev/kurn/engine"
)

// MaxRecordBytes bounds one source record, matching the engine's journal/
// base line bound: an entry the store would refuse must fail at ingest.
// It is enforced twice, because the two sizes differ: on the source
// record as parsed, and again on the entry as serialized (checkEntrySize)
// — JSON escaping can expand a legal record past the bound.
const MaxRecordBytes = 1 << 20

// Mapping declares how one feed becomes one list.
type Mapping struct {
	// Format selects the parser: "ndjson" | "csv" | "xml".
	Format string `json:"format"`
	// Record is the XML record element's local name (e.g. "sdnEntry");
	// required for xml, forbidden otherwise.
	Record string `json:"record,omitempty"`
	// Delimiter overrides the CSV field separator (default ","). One
	// rune; European publications commonly use ";".
	Delimiter string `json:"delimiter,omitempty"`
	// Where filters whole records: every path must have at least one
	// match equal to the given value, or the record is (silently,
	// counted) filtered out. E.g. {"sdnType": "Individual"}. The value
	// "*" matches any NON-EMPTY value — the way to select rows of a
	// denormalized feed that actually carry the thing you are mapping
	// (a literal "*" in the data cannot be matched; no feed has needed
	// it, and filtered records are counted, never silently dropped).
	Where map[string]string `json:"where,omitempty"`
	// ID is the entry-ID path (CSV: the column name, used verbatim).
	ID string `json:"id,omitempty"`
	// IDPaths builds a COMPOSITE entry ID: the first match of each path,
	// trimmed, joined with IDJoin — empties kept as empty components so
	// the ID stays positionally stable. For feeds with no stable
	// single-column ID (LEIE has none: NPI is zero-filled when absent and
	// not unique when present). Exactly one of ID / IDPaths. The
	// stable-ID requirement then rests on the FIELD CHOICE: pick columns
	// that identify the fact, not ones that drift between releases.
	IDPaths []string `json:"id_paths,omitempty"`
	// IDJoin separates composite components (default "|").
	IDJoin *string `json:"id_join,omitempty"`
	// Keys are the key-extraction rules; a record must yield at least one
	// non-empty key or it is a bad record.
	Keys []KeyRule `json:"keys"`
	// Payload builds the opaque payload object: field name -> path.
	// Single match -> string, multiple -> array, none -> field omitted.
	Payload map[string]string `json:"payload,omitempty"`
	// List is the full list configuration the built list will carry.
	List engine.ListConfig `json:"list"`
}

// KeyRule extracts zero or more keys from a record. The INSTANCE set is
// defined by the deepest common path prefix of Paths and the Where paths:
// for {paths: [akaList.aka.firstName, akaList.aka.lastName], where:
// {akaList.aka.type: "a.k.a."}} the instances are the individual aka
// elements — the filter and the joined name components are evaluated per
// aka, never across siblings. One key is emitted per instance that passes
// every Where condition: the first match of each path, empties skipped,
// joined with Join.
//
// CAUTION — the instance set follows from the paths you write. With
//
//	{paths: [akaList.aka.lastName], where: {akaList.aka.type: "strong"}}
//
// the common prefix is akaList.aka: the filter is PER aka (only strong
// akas' names). But with
//
//	{paths: [lastName], where: {akaList.aka.type: "strong"}}
//
// the common prefix is empty: the instance is the whole record, and the
// filter degrades to an existential — "emit lastName if the record has ≥1
// strong aka ANYWHERE". Both are well-defined; only one is usually what
// you meant. If a filter should constrain the same repeated element the
// keys come from, the where paths must share that element's prefix.
type KeyRule struct {
	// Path is sugar for a single-element Paths.
	Path string `json:"path,omitempty"`
	// Paths are joined per instance (e.g. firstName + lastName).
	Paths []string `json:"paths,omitempty"`
	// Join separates the components (default " ").
	Join *string `json:"join,omitempty"`
	// Where are equals-only conditions evaluated per instance.
	Where map[string]string `json:"where,omitempty"`
	// Split, when set, breaks the extracted value on this separator and
	// emits ONE KEY PER PIECE — the shape CSV feeds use for aliases
	// ("A; B; C" in a single column). Pieces are trimmed and empties
	// dropped. Without it such a column becomes one long key that
	// matches nothing well.
	Split string `json:"split,omitempty"`
}

func (k *KeyRule) paths() []string {
	if k.Path != "" {
		return []string{k.Path}
	}
	return k.Paths
}

func (k *KeyRule) join() string {
	if k.Join == nil {
		return " "
	}
	return *k.Join
}

// Validate checks the mapping's shape, including that List is a
// configuration the engine would accept — a mapping that parses cleanly
// but builds an invalid list must fail here, not at the end of a 10 GB
// parse.
func (m *Mapping) Validate() error {
	switch m.Format {
	case "ndjson", "csv":
		if m.Record != "" {
			return fmt.Errorf("ingest: record is xml-only (format %q)", m.Format)
		}
		if m.Delimiter != "" {
			if m.Format != "csv" {
				return fmt.Errorf("ingest: delimiter is csv-only (format %q)", m.Format)
			}
			if utf8.RuneCountInString(m.Delimiter) != 1 {
				return fmt.Errorf("ingest: delimiter must be exactly one rune, got %q", m.Delimiter)
			}
		}
	case "xml":
		if m.Record == "" {
			return fmt.Errorf("ingest: xml requires a record element name")
		}
		if m.Delimiter != "" {
			return fmt.Errorf("ingest: delimiter is csv-only (format xml)")
		}
	default:
		return fmt.Errorf("ingest: unknown format %q (want ndjson, csv, or xml)", m.Format)
	}
	if (m.ID == "") == (len(m.IDPaths) == 0) {
		return fmt.Errorf("ingest: exactly one of id or id_paths is required")
	}
	for i, p := range m.IDPaths {
		if p == "" {
			return fmt.Errorf("ingest: id_paths[%d]: empty path", i)
		}
	}
	if len(m.Keys) == 0 {
		return fmt.Errorf("ingest: at least one key rule is required")
	}
	for i, k := range m.Keys {
		if (k.Path != "") == (len(k.Paths) > 0) {
			return fmt.Errorf("ingest: keys[%d]: exactly one of path or paths", i)
		}
		for _, p := range k.paths() {
			if p == "" {
				return fmt.Errorf("ingest: keys[%d]: empty path", i)
			}
		}
		for w := range k.Where {
			if w == "" {
				return fmt.Errorf("ingest: keys[%d]: empty where path", i)
			}
		}
		if k.Split != "" && len(k.paths()) > 1 {
			return fmt.Errorf("ingest: keys[%d]: split applies to a single path (joining then splitting is ambiguous)", i)
		}
	}
	if _, err := engine.NewList("mappingcheck", m.List); err != nil {
		return fmt.Errorf("ingest: list config: %w", err)
	}
	return nil
}

// Options tune a parse run.
type Options struct {
	// SkipBad tolerates up to this many bad records (missing ID, no keys,
	// oversize, malformed), counting them in Stats. The default 0 makes
	// the first bad record fail the run — silent drops are the enemy.
	SkipBad int
}

// Stats reports what a parse run saw.
type Stats struct {
	Records  int // entries yielded
	Filtered int // records the mapping's Where excluded (by design)
	Bad      int // bad records skipped (only when Options.SkipBad allows)
}

// badRecord is an input-data problem attributable to one record.
type badRecord struct{ err error }

func (b badRecord) Error() string { return b.err.Error() }

// Parse streams records from r through the mapping, yielding one entry
// per passing record. A yield error aborts the run. Bad records fail the
// run unless Options.SkipBad tolerates them.
func Parse(m *Mapping, r io.Reader, opts Options, yield func(engine.Entry) error) (Stats, error) {
	if err := m.Validate(); err != nil {
		return Stats{}, err
	}
	var st Stats
	handle := func(doc any, recNo int) error {
		e, filtered, err := m.mapRecord(doc)
		if err == nil && !filtered {
			err = checkEntrySize(e)
		}
		if err != nil {
			if st.Bad < opts.SkipBad {
				st.Bad++
				return nil
			}
			return fmt.Errorf("ingest: record %d: %w (bad records so far: %d; raise -skip-bad to tolerate)", recNo, err, st.Bad)
		}
		if filtered {
			st.Filtered++
			return nil
		}
		st.Records++
		return yield(e)
	}
	var err error
	switch m.Format {
	case "ndjson":
		err = parseNDJSON(r, m, handle)
	case "csv":
		err = parseCSV(r, m, handle)
	case "xml":
		err = parseXML(r, m, handle)
	}
	return st, err
}

// checkEntrySize enforces MaxRecordBytes on the SERIALIZED entry, which is
// what the store actually bounds. A record inside the bound can still cross
// it once mapped: JSON escaping turns one control byte into six (\u0000),
// so a field of them grows about sixfold. Without this the entry is refused
// by writeBaseTemp instead — after the whole feed is parsed and resident,
// naming no record number and skippable by nothing.
//
// The sixfold gate keeps json.Marshal off the common path: below it no
// amount of escaping can reach the bound, and real feed records are three
// orders of magnitude below it.
func checkEntrySize(e engine.Entry) error {
	raw := len(e.ID) + len(e.Payload)
	for _, k := range e.Keys {
		raw += len(k) + 3 // quotes and separator
	}
	if 6*raw+64 <= MaxRecordBytes {
		return nil
	}
	b, err := json.Marshal(&e)
	if err != nil {
		return badRecord{err}
	}
	if len(b)+1 > MaxRecordBytes {
		return badRecord{fmt.Errorf("entry serializes to %d bytes, over the %d-byte bound (json escaping expands control bytes sixfold)", len(b)+1, MaxRecordBytes)}
	}
	return nil
}

// mapRecord applies the mapping to one decoded record. filtered=true means
// the record-level Where excluded it (not an error, not an entry).
func (m *Mapping) mapRecord(doc any) (e engine.Entry, filtered bool, err error) {
	if bd, ok := doc.(badDoc); ok {
		return engine.Entry{}, false, badRecord{bd.err}
	}
	for wpath, want := range m.Where {
		if !anyEquals(resolve(doc, splitPath(m.Format, wpath)), want) {
			return engine.Entry{}, true, nil
		}
	}
	if len(m.IDPaths) > 0 {
		parts := make([]string, len(m.IDPaths))
		empty := true
		for i, p := range m.IDPaths {
			if nodes := resolve(doc, splitPath(m.Format, p)); len(nodes) > 0 {
				parts[i] = strings.TrimSpace(text(nodes[0]))
			}
			if parts[i] != "" {
				empty = false
			}
		}
		if empty {
			return engine.Entry{}, false, badRecord{fmt.Errorf("composite id empty (all of %v)", m.IDPaths)}
		}
		join := "|"
		if m.IDJoin != nil {
			join = *m.IDJoin
		}
		e.ID = strings.Join(parts, join)
	} else {
		ids := texts(resolve(doc, splitPath(m.Format, m.ID)))
		if len(ids) == 0 || strings.TrimSpace(ids[0]) == "" {
			return engine.Entry{}, false, badRecord{fmt.Errorf("missing id at %q", m.ID)}
		}
		e.ID = strings.TrimSpace(ids[0])
	}

	seen := map[string]struct{}{}
	for _, rule := range m.Keys {
		for _, k := range applyRule(doc, m.Format, rule) {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			e.Keys = append(e.Keys, k)
		}
	}
	if len(e.Keys) == 0 {
		return engine.Entry{}, false, badRecord{fmt.Errorf("no keys extracted (id %s)", e.ID)}
	}

	if len(m.Payload) > 0 {
		obj := map[string]any{}
		fields := make([]string, 0, len(m.Payload))
		for f := range m.Payload {
			fields = append(fields, f)
		}
		sort.Strings(fields) // deterministic payload bytes
		for _, f := range fields {
			vals := texts(resolve(doc, splitPath(m.Format, m.Payload[f])))
			switch len(vals) {
			case 0:
			case 1:
				obj[f] = vals[0]
			default:
				obj[f] = vals
			}
		}
		if len(obj) > 0 {
			raw, merr := json.Marshal(obj)
			if merr != nil {
				return engine.Entry{}, false, badRecord{merr}
			}
			e.Payload = raw
		}
	}
	return e, false, nil
}
