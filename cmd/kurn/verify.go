// kurn verify — check a prepared artifact file: structure, digests and the
// logical base ID (level "full"), and with -semantic also that the index,
// configuration identity and derivable counters are what the records
// produce.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

// Exit codes of kurn verify. main's usual mapping (every error exits 1)
// cannot carry them, so its "verify" case exits with verifyExit's code.
const (
	verifyExitOK        = 0 // everything requested verified
	verifyExitOther     = 1 // I/O and any other error
	verifyExitUsage     = 2 // bad flag, missing FILE, extra arguments
	verifyExitIntegrity = 3 // digest, identity or semantic mismatch (prepared.ErrIntegrity)
	verifyExitMalformed = 4 // malformed or incompatible artifact
)

// verifyUsageError marks a command-line usage error (exit 2).
type verifyUsageError struct{ msg string }

func (e *verifyUsageError) Error() string { return "verify: " + e.msg }

const verifyNoAuthNote = "note: verification shows the artifact is self-consistent; it does not authenticate who produced it"

// cmdVerify is `kurn verify [-semantic] FILE`.
func cmdVerify(args []string) error { return verifyTo(args, os.Stdout, os.Stderr) }

// verifyExit prints err as main does and returns kurn verify's exit code.
func verifyExit(err error) int { return verifyExitTo(err, os.Stderr) }

func verifyExitTo(err error, stderr io.Writer) int {
	if err == nil {
		return verifyExitOK
	}
	fmt.Fprintln(stderr, "kurn:", err)
	return verifyExitCode(err)
}

// verifyExitCode maps a verify error to its exit code.
func verifyExitCode(err error) int {
	var ue *verifyUsageError
	switch {
	case err == nil:
		return verifyExitOK
	case errors.As(err, &ue):
		return verifyExitUsage
	case errors.Is(err, prepared.ErrIntegrity):
		return verifyExitIntegrity
	case errors.Is(err, prepared.ErrMalformed), errors.Is(err, prepared.ErrIncompatible), errors.Is(err, prepared.ErrUnaligned):
		return verifyExitMalformed
	}
	return verifyExitOther
}

// verifyTo runs kurn verify. The report is written to stdout only once the
// verdict is known and only on success; on failure nothing reaches stdout,
// the caller prints the error, and a semantic mismatch's details go to
// stderr first.
func verifyTo(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	semantic := fs.Bool("semantic", false, "also rebuild the index from the records and compare it byte for byte, recompute the config and analyzer digests, and recompute the derivable loss counters")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: kurn verify [-semantic] FILE")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &verifyUsageError{msg: err.Error()}
	}
	switch fs.NArg() {
	case 0:
		fs.Usage()
		return &verifyUsageError{msg: "missing FILE"}
	case 1:
	default:
		fs.Usage()
		return &verifyUsageError{msg: fmt.Sprintf("expected one FILE, got %d arguments", fs.NArg())}
	}
	path := fs.Arg(0)

	var out bytes.Buffer
	if !*semantic {
		ver, err := prepared.Verify(path, prepared.LevelFull)
		if err != nil {
			return fmt.Errorf("verify %s: %w", path, err)
		}
		writeVerified(&out, path, ver, "full")
		fmt.Fprintln(&out, "declared, not recomputed at this level (use -semantic):")
		fmt.Fprintf(&out, "  config digest     %s\n", ver.ConfigDigest)
		fmt.Fprintf(&out, "  analyzer digest   %s\n", ver.AnalyzerDigest)
		fmt.Fprintln(&out, verifyNoAuthNote)
		_, err = stdout.Write(out.Bytes())
		return err
	}

	rep, err := builder.VerifySemantic(path)
	if err != nil {
		if rep != nil {
			fmt.Fprintf(stderr, "SEMANTIC VERIFICATION FAILED for %s\n", path)
			writeSemantic(stderr, rep)
			fmt.Fprintln(stderr, verifyNoAuthNote)
		}
		return fmt.Errorf("verify %s: %w", path, err)
	}
	if !rep.OK() { // unreachable: a failed check always comes with an error
		return fmt.Errorf("verify %s: %w: semantic verification incomplete", path, prepared.ErrIntegrity)
	}
	writeVerified(&out, path, &rep.Verified, "full + semantic")
	writeSemantic(&out, rep)
	fmt.Fprintln(&out, verifyNoAuthNote)
	_, err = stdout.Write(out.Bytes())
	return err
}

func writeVerified(w io.Writer, path string, v *prepared.Verified, level string) {
	fmt.Fprintf(w, "verified %s at level %q\n", path, level)
	fmt.Fprintf(w, "  list              %s\n", v.Name)
	fmt.Fprintf(w, "  mode              %s\n", v.Mode)
	if v.ExactCompanion {
		fmt.Fprintf(w, "  exact companion   yes (section kind %d)\n", prepared.KindExactCompanion)
	}
	fmt.Fprintf(w, "  format            %s %d.%d\n", prepared.Magic, v.Major, v.Minor)
	fmt.Fprintf(w, "  entries           %d\n", v.Entries)
	fmt.Fprintf(w, "  keys              %d\n", v.Keys)
	fmt.Fprintf(w, "  base id           %s (recomputed from the records)\n", v.BaseID)
	fmt.Fprintf(w, "  container sha256  %s\n", v.ContainerSHA256)
}

func okWord(ok bool) string {
	if ok {
		return "ok"
	}
	return "MISMATCH"
}

// writeSemantic prints the semantic checks, then the declared-only fields.
func writeSemantic(w io.Writer, r *builder.SemanticReport) {
	fmt.Fprintln(w, "semantic checks (rebuilt from the records with the engine):")
	fmt.Fprintf(w, "  config digest     %s  declared %s, recomputed %s\n", okWord(r.ConfigDigestOK), r.Verified.ConfigDigest, r.RecomputedConfigDigest)
	fmt.Fprintf(w, "  analyzer digest   %s  declared %s, recomputed %s\n", okWord(r.AnalyzerDigestOK), r.Verified.AnalyzerDigest, r.RecomputedAnalyzerDigest)
	if r.IndexOK {
		fmt.Fprintf(w, "  index (%s)     ok  byte-identical to the rebuild\n", r.IndexKind)
	} else {
		fmt.Fprintf(w, "  index (%s)     MISMATCH  first differing byte at section offset %d: %s\n", r.IndexKind, r.IndexMismatchOffset, r.IndexMismatch)
	}
	if r.Verified.ExactCompanion {
		if r.CompanionOK {
			fmt.Fprintln(w, "  exact companion   ok  byte-identical to the rebuild")
		} else {
			fmt.Fprintf(w, "  exact companion   MISMATCH  %s\n", r.CompanionMismatch)
		}
	}
	for _, c := range r.Counters {
		fmt.Fprintf(w, "  %-17s %s  declared %d, recomputed %d\n", c.Name, okWord(c.OK), c.Declared, c.Recomputed)
	}
	for _, s := range r.Sections {
		if !s.OK {
			fmt.Fprintf(w, "  %s section differs from the rebuild at section offset %d (file offset %d)\n", s.Kind, s.Offset, s.FileOffset)
		}
	}
	fmt.Fprintln(w, "declared, not verifiable from the artifact:")
	fmt.Fprintf(w, "  source            %q\n", r.Declared.Source)
	fmt.Fprintf(w, "  created at        %q\n", r.Declared.CreatedAt)
	fmt.Fprintf(w, "  input sha256      %q\n", r.Declared.InputSHA256)
	fmt.Fprintf(w, "  collapsed         %d\n", r.Declared.Collapsed)
	if len(r.Declared.UnknownSections) > 0 {
		fmt.Fprintf(w, "  unknown optional sections (digest-covered, not interpreted): %v\n", r.Declared.UnknownSections)
	}
}
