# Example mappings

Each file is a complete declarative mapping: `kurn build -mapping <file>
-in <feed> -out <dir>` turns the official download into a publishable
bundle. Public feed data itself stays OUT of this repo (see the
test-data doc) — real-feed tests are env-gated on a locally downloaded
copy.

| Mapping | Feed | Source | Cadence | License / access |
|---|---|---|---|---|
| `ofac.mapping.json` | OFAC SDN (individuals) | treasury.gov sanctions list service | ~daily | US gov, public |
| `leie.mapping.json` | OIG LEIE exclusions | oig.hhs.gov/exclusions (UPDATED.csv) | monthly + supplements | US gov, public domain |
| `sam-exclusions.mapping.json` | SAM.gov exclusions (Public Extract V2) | sam.gov Data Services | daily | US gov public data; DOWNLOAD requires a free SAM account |
| `un-consolidated.mapping.json` | UN Security Council Consolidated List (individuals) | scsanctions.un.org XML | as amended | UN publication, freely downloadable |

## LEIE notes

- **No stable single-column ID exists**: `NPI` is zero-filled when
  absent (~90% of rows) and not unique when present; there is no row
  key. The mapping uses `id_paths` over all fifteen data columns — the
  ID *is* the fact, so it is stable exactly as long as the record is
  unchanged, which is what the delta pipeline wants (a changed record is
  a delete+add, which is the truth of it).
- **The feed ships literal duplicate rows** (~26 byte-identical pairs
  in the 2026-07 file). The builder collapses byte-identical duplicates
  and counts them in the manifest (`collapsed`); an ID naming two
  DIFFERENT records still fails the build.
- Persons carry `LASTNAME/FIRSTNAME/MIDNAME`, businesses `BUSNAME` —
  two key rules, empties skipped, so every record yields exactly its
  applicable name key.
- Env-gated real-feed test: `KURN_LEIE_CSV=/path/to/UPDATED.csv`.

## SAM notes

- `SAM Number` is a real per-record key, so no composite is needed.
- The extract download sits behind a free SAM.gov account (the DATA is
  public, the tap is gated). Column headers in this mapping follow the
  documented Public Extract V2 layout — **verify against a freshly
  downloaded extract before production use** (env-gated test:
  `KURN_SAM_CSV=/path/to/extract.csv`).

## UN consolidated notes

- Record element `INDIVIDUAL`; `DATAID` is a real per-record id.
- Names arrive as up to four parts (FIRST_NAME…FOURTH_NAME) plus
  `INDIVIDUAL_ALIAS.ALIAS_NAME` and `NAME_ORIGINAL_SCRIPT` — all three
  rules feed the same list, so an alias or the original-script form
  matches as readily as the primary name.
- Entities (`<ENTITY>`) are a separate record element and are NOT built
  by this mapping; a second mapping can serve them when wanted.
- Measured 2026-08-01: 736 individuals, 3,215 keys.
