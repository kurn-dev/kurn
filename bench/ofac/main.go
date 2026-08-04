// Command ofac extracts the OFAC SDN list into the bench harness's file
// shapes (a re-runnable ground-truth experiment): entries NDJSON for
// `kurn bench -entries` and an alias query corpus CSV for `-corpus`.
//
// The SDN data itself is NOT in the repo: US government work (public
// domain), but list snapshots change daily and belong outside version
// control. Download first:
//
//	curl -sfL -o sdn.xml "https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.XML"
//	go run ./bench/ofac -in sdn.xml -entries ofac-entries.jsonl -corpus ofac-corpus.csv
//	go run ./cmd/kurn bench -entries ofac-entries.jsonl -corpus ofac-corpus.csv -threshold 0.6
//
// Extraction rules (the experiment's definitions):
//   - Individuals only (sdnType == "Individual").
//   - Primary name: firstName + lastName (token order is irrelevant — the
//     person-name preset sorts tokens).
//   - Query corpus: strong aliases (category "strong", type "a.k.a." or
//     "f.k.a.") whose letters are all Latin script, excluding aliases equal
//     to their primary name (those are EXACT queries, not alias bridging).
//     Weak a.k.a.s are partial names and non-Latin originals need
//     transliteration — both out of scope for the string-bridging question
//     the gate asked.
//   - Held-out mode (default): entries carry ONLY the primary name, so the
//     matcher itself must bridge primary↔alias. With -index-aliases the
//     aliases ride as keys too (how screening lists actually ship) — recall
//     is then ~1.0 by construction, the deployment-shaped control.
package main

import (
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"strings"
	"unicode"
)

type aka struct {
	Type      string `xml:"type"`
	Category  string `xml:"category"`
	FirstName string `xml:"firstName"`
	LastName  string `xml:"lastName"`
}

type sdnEntry struct {
	UID       string `xml:"uid"`
	SDNType   string `xml:"sdnType"`
	FirstName string `xml:"firstName"`
	LastName  string `xml:"lastName"`
	AKAs      []aka  `xml:"akaList>aka"`
}

func fullName(first, last string) string {
	return strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
}

// latinScript reports whether every letter in s is Latin script (digits,
// spaces, punctuation are fine — real aliases carry hyphens and quotes).
func latinScript(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) && !unicode.Is(unicode.Latin, r) {
			return false
		}
	}
	return true
}

func main() {
	in := flag.String("in", "sdn.xml", "OFAC SDN XML path (download separately; see file comment)")
	entriesPath := flag.String("entries", "ofac-entries.jsonl", "entries NDJSON output (kurn bench -entries)")
	corpusPath := flag.String("corpus", "ofac-corpus.csv", "alias query corpus CSV output (kurn bench -corpus)")
	indexAliases := flag.Bool("index-aliases", false, "also index aliases as entry keys (deployment-shaped control; default holds them out)")
	flag.Parse()

	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ofac:", err)
		os.Exit(1)
	}
	defer f.Close()

	ef, err := os.Create(*entriesPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ofac:", err)
		os.Exit(1)
	}
	cf, err := os.Create(*corpusPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ofac:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(ef)
	cw := csv.NewWriter(cf)
	cw.Write([]string{"query", "truth_id", "category"})

	type entryOut struct {
		ID   string   `json:"id"`
		Keys []string `json:"keys"`
	}

	dec := xml.NewDecoder(f)
	individuals, aliases := 0, 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break // io.EOF or a truncated file: report what was parsed
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sdnEntry" {
			continue
		}
		var e sdnEntry
		if err := dec.DecodeElement(&e, &se); err != nil {
			fmt.Fprintln(os.Stderr, "ofac: decode:", err)
			os.Exit(1)
		}
		if e.SDNType != "Individual" {
			continue
		}
		primary := fullName(e.FirstName, e.LastName)
		if primary == "" {
			continue
		}
		individuals++
		keys := []string{primary}
		for _, a := range e.AKAs {
			if (a.Type != "a.k.a." && a.Type != "f.k.a.") || a.Category != "strong" {
				continue
			}
			name := fullName(a.FirstName, a.LastName)
			if name == "" || !latinScript(name) || strings.EqualFold(name, primary) {
				continue
			}
			aliases++
			cw.Write([]string{name, e.UID, "ALIAS"})
			if *indexAliases {
				keys = append(keys, name)
			}
		}
		enc.Encode(entryOut{ID: e.UID, Keys: keys})
	}
	cw.Flush()
	if err := ef.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "ofac:", err)
		os.Exit(1)
	}
	if err := cf.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "ofac:", err)
		os.Exit(1)
	}
	fmt.Printf("ofac: %d individuals, %d strong Latin-script aliases -> %s, %s (aliases indexed: %v)\n",
		individuals, aliases, *entriesPath, *corpusPath, *indexAliases)
}
