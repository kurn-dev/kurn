package engine_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
)

// (c) C1: an artifact whose declared identities do not match its own
// configuration passes every digest of the format (it was rehashed) and is
// still refused by the engine, which recomputes both identities itself —
// at every verification level. A configuration the engine cannot resolve
// at all is refused as incompatible.
func TestPreparedIdentitiesRecomputed(t *testing.T) {
	flip := func(hex64 string) string {
		b := []byte(hex64)
		if b[0] == '0' {
			b[0] = '1'
		} else {
			b[0] = '0'
		}
		return string(b)
	}
	c := prepCases()[2] // ngram with filterable fields
	ok := prepEncode(t, c)
	_, ver, err := prepared.VerifyBytes(ok, prepared.LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, old, new, want string
		is                   error
	}{
		{"config digest", `"config_digest":"` + ver.ConfigDigest, `"config_digest":"` + flip(ver.ConfigDigest), "config digest", prepared.ErrIntegrity},
		{"analyzer digest", `"analyzer_digest":"` + ver.AnalyzerDigest, `"analyzer_digest":"` + flip(ver.AnalyzerDigest), "analyzer digest", prepared.ErrIntegrity},
		// The configuration changes under an unchanged declared digest.
		{"config changed", `"threshold":0.6`, `"threshold":0.7`, "config digest", prepared.ErrIntegrity},
		// A configuration the engine cannot resolve is ErrIncompatible
		// (as builder.VerifySemanticBytes classifies it): an unknown field
		// fails the strict decode ...
		{"config unknown field", `"strip_spaces":true`, `"strip_spacez":true`, "cannot resolve the artifact's list configuration: json: unknown field", prepared.ErrIncompatible},
		// ... NewList refuses an out-of-range threshold ...
		{"config refused by NewList", `"threshold":0.6`, `"threshold":7.6`, "cannot resolve the artifact's list configuration: list watch: match threshold", prepared.ErrIncompatible},
		// ... and a form declared out of resolved order would be normalized
		// by NewList, so the stored form is not the served configuration.
		{"config not resolved", `[{"name":"country","path":"meta.country"},{"name":"program","path":"program"}]`,
			`[{"name":"program","path":"program"},{"name":"country","path":"meta.country"}]`, "cannot resolve the artifact's list configuration: it is not in the resolved canonical form", prepared.ErrIncompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := prepared.AlignedBuffer(len(ok))
			copy(buf, ok)
			prepPatchSection(t, buf, prepared.KindConfig, tc.old, tc.new)
			prepRehash(buf)
			if _, _, err := prepared.VerifyBytes(buf, prepared.LevelFull); err != nil {
				t.Fatalf("the patched artifact must pass the format's own full verification: %v", err)
			}
			path := prepWriteBytes(t, buf)
			for _, lvl := range []prepared.Level{prepared.LevelFull, prepared.LevelStructure} {
				st, err := engine.OpenPrepared(path, engine.PreparedOptions{Verify: lvl})
				if st != nil || !errors.Is(err, tc.is) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("level %v: store %v err %v, want %v naming %q", lvl, st, err, tc.is, tc.want)
				}
			}
		})
	}
}

// (e) Corruption fails full verification with ErrIntegrity, malformed
// bytes fail with ErrMalformed, and a failed open returns no store.
func TestPreparedCorruptAndMalformed(t *testing.T) {
	c := prepCases()[0]
	ok := prepEncode(t, c)
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
		is     error
	}{
		{"payload bit flip", func(b []byte) []byte {
			s := prepSections(b)[prepared.KindRecords]
			i := bytes.Index(b[s[0]:s[0]+s[1]], []byte("SDN"))
			b[s[0]+uint64(i)+1] ^= 0x01 // D → E
			return b
		}, prepared.ErrIntegrity},
		{"index bit flip", func(b []byte) []byte {
			s := prepSections(b)[prepared.KindNgram]
			b[s[0]+s[1]-1] ^= 0x80
			return b
		}, nil}, // integrity or malformed, depending on what the bit was
		{"config bit flip", func(b []byte) []byte {
			s := prepSections(b)[prepared.KindConfig]
			i := bytes.Index(b[s[0]:s[0]+s[1]], []byte(`"config_digest":"`)) + len(`"config_digest":"`)
			if b[s[0]+uint64(i)] == '0' { // stay a hex digit: only the digest can tell
				b[s[0]+uint64(i)] = '1'
			} else {
				b[s[0]+uint64(i)] = '0'
			}
			return b
		}, prepared.ErrIntegrity},
		{"header digest", func(b []byte) []byte {
			b[24+64*int(binary.LittleEndian.Uint32(b[16:]))] ^= 1 // first byte of the header digest
			return b
		}, prepared.ErrIntegrity},
		{"bad magic", func(b []byte) []byte { b[0] = 'X'; return b }, prepared.ErrMalformed},
		{"truncated", func(b []byte) []byte { return b[:len(b)-1] }, prepared.ErrMalformed},
		{"trailing byte", func(b []byte) []byte { return append(b, 0) }, prepared.ErrMalformed},
		{"empty", func(b []byte) []byte { return nil }, prepared.ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), ok...)
			path := prepWriteBytes(t, tc.mutate(b))
			st, err := engine.OpenPrepared(path, engine.PreparedOptions{})
			if st != nil || err == nil {
				t.Fatalf("store %v err %v, want a refusal and no store", st, err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("%v is not %v", err, tc.is)
			}
			if tc.is == nil && !errors.Is(err, prepared.ErrIntegrity) && !errors.Is(err, prepared.ErrMalformed) {
				t.Fatalf("%v is neither integrity nor malformed", err)
			}
		})
	}
	if st, err := engine.OpenPrepared(prepWriteBytes(t, ok)+".missing", engine.PreparedOptions{}); st != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: store %v err %v", st, err)
	}
}

// (d) The heap loader keeps nothing of the file: overwriting or removing
// it after the open changes no answer.
func TestPreparedHeapIsolation(t *testing.T) {
	for _, c := range prepCases()[:2] {
		t.Run(c.name, func(t *testing.T) {
			path := prepWriteArtifact(t, c)
			st := prepOpen(t, path, engine.PreparedOptions{})
			l := prepList(t, st)
			want := prepAnswers(t, l, c)
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			junk := bytes.Repeat([]byte{0xA5}, int(fi.Size()))
			if err := os.WriteFile(path, junk, 0o644); err != nil {
				t.Fatal(err)
			}
			if got := prepAnswers(t, l, c); !bytes.Equal(got, want) {
				t.Fatalf("answers changed after the file was overwritten:\n%s", prepFirstDiff(got, want))
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if got := prepAnswers(t, l, c); !bytes.Equal(got, want) {
				t.Fatal("answers changed after the file was removed")
			}
		})
	}
}
