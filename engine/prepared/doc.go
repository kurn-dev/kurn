// Package prepared is the prepared artifact format: one immutable file
// holding a single list's resolved configuration, build provenance, records
// and base index, so a server can load a list without parsing base.jsonl or
// rebuilding its index.
//
// The package is deliberately engine-free (it imports engine/ngram and
// engine/exact, never engine itself) because the engine's prepared loader
// imports it. Building an artifact from engine entries with the engine's own
// index code lives in the builder subpackage.
//
// # Two identities
//
// The logical base identity is the engine's base ID: the SHA-256 of the
// canonical base.jsonl stream, json.Marshal(&entry) plus "\n" for every entry
// in base order. It is continuous with bundles and version stamps. The
// container identity is the SHA-256 of the whole artifact file and names the
// file, nothing else. Full verification recomputes the logical identity from
// the records section; a declared base ID the records do not reproduce is an
// integrity error.
//
// # Binary layout (format 1.0, little-endian throughout)
//
//	header   magic "KURNART1" | major u16 | minor u16 | flags u32 (zero)
//	         | section count u32 | reserved u32 (zero)            24 bytes
//	table    count × 64-byte entries: kind u32 | flags u32 (bit 0 =
//	         required, other bits zero) | offset u64 | length u64
//	         | SHA-256 of the section (32) | reserved u64 (zero)
//	digest   SHA-256 over header and table                         32 bytes
//	sections each at a 32-byte aligned file offset, in table order with
//	         strictly increasing offsets, never overlapping; every byte
//	         between the digest and the first section and between sections
//	         is zero; the file ends exactly at the end of the last section.
//	         Kinds are unique. An unknown kind flagged required, or an
//	         unknown major version, is an incompatibility error.
//
// Section kinds and their bodies (offsets inside a section are relative to
// the blob they index, lengths are byte counts):
//
//	1 config   canonical JSON {"name","analyzer_digest","config_digest","list"}
//	           where list is the engine's resolved ListConfig as json.Marshal
//	           writes it.
//	2 build    canonical JSON: logical base ID, entry and key counts, the
//	           build's loss counters, provenance.
//	3 records  N u64 | N rows of six u64 (id off, id len, key start, key
//	           count, payload off, payload len) | K u64 | K rows of two u64
//	           (key off, key len) | blob. The blob is packed canonically:
//	           for each record in order its ID bytes, then its keys' bytes,
//	           then its payload bytes, with no gaps. Bit 63 of a key count
//	           marks a JSON null key list and requires a zero count. Payloads
//	           are the exact bytes of the canonical base line.
//	4 ids      N u64 | N u64 ordinals, ordered so the records' IDs are
//	           strictly increasing bytewise.
//	5 ngram    G u64 | G rows of four u64 (gram off, gram len, bitmap off,
//	           bitmap len) | numOrds u64 | gram blob length u64 | gram blob
//	           | zero padding to a 32-byte boundary | bitmap blob. Grams are
//	           strictly increasing bytewise and packed without gaps. Each
//	           bitmap is roaring's frozen format (FreezeTo) at a 32-byte
//	           aligned offset of the bitmap blob, packed in gram order with
//	           zero padding; the last ends at the section end.
//	6 exact    K u64 | P u64 | arena length u64 | K u32 key lengths | K u32
//	           run lengths | P u32 postings | arena. Keys are strictly
//	           increasing bytewise; postings ascend strictly inside a run.
//
// # What the base ID binds
//
// Every record ID and key must be valid UTF-8 (Encode refuses anything else,
// and NewView treats it as malformed at every level). For valid UTF-8
// strings the canonical encoding is injective — json.Marshal's escaping is
// reversible and it rewrites no bytes — so the recomputed base ID binds the
// exact ID and key bytes the View serves. (For invalid UTF-8 it would not:
// json.Marshal replaces each invalid byte with U+FFFD, so distinct bytes
// would hash alike.) Payloads are bound bytewise too: full verification
// requires each stored payload to equal its own canonical form, which is the
// exact span of the hashed line.
//
// The base ID binds the records only. The index sections (ngram, exact) are
// bound by their section digests and the container digest, and structurally
// checked (ordinals in range, numOrds, sorted grams and keys, well-formed
// bitmaps and runs), but nothing here checks that the index is the one the
// records and configuration produce: that takes a rebuild. Likewise the
// build section's loss counters are declared, not recomputed, and the config
// digests are recomputed only by the engine (builder.ListConfig).
//
// # Validation
//
// NewView validates everything structural, every time, whatever digests say:
// digests show that bytes are unchanged since someone hashed them, not who
// produced them. Every offset, length and count is bounds- and overflow-
// checked before use, and each embedded frozen bitmap passes this package's
// own structural check before roaring's FrozenView sees it. Full
// verification (the default level) additionally checks the header and every
// section digest, recomputes the logical base ID from the records, and
// computes the container digest.
//
// Trusted verification (LevelTrusted, an explicit operator-trusted serving
// policy) keeps every structural check, the header and table digest, the
// digests of every section except the records, ids and index sections, and
// a JSON syntax check of every non-empty payload; it skips the content
// work: those three section digests, the base-ID recomputation with its
// canonical-payload check, and the container digest. It establishes no
// verified identity (BaseID and ContainerSHA256 stay empty) and cannot
// detect content corruption that stays structurally valid. The header
// digest shows consistency between the header and the section table, not
// authenticity. Trusted verification is not size-independent. Structure
// verification (LevelStructure) is a diagnostic that runs NewView only.
//
// # Borrowed memory
//
// A View never copies: IDs and keys are unsafe.String views, payloads and
// postings are slices, and bitmaps are FrozenView views, all over the
// caller's buffer. The caller must keep the buffer unmodified for as long as
// any view derived from it is reachable. Payload slices are capped at their
// own length, which stops append from reaching neighbouring bytes but does
// not make them immutable. Bitmaps from FrozenView must never be mutated:
// roaring's copy-on-write marker protects containers, but its keys slice
// aliases the buffer and an insert of a new high key would write into it.
package prepared
