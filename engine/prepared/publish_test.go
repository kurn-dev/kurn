package prepared

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func readDirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestPublishCreatesReadOnlyFileAndCleansUp(t *testing.T) {
	buf := artifactT(t, "exact", 20)
	dir := t.TempDir()
	dst := filepath.Join(dir, "list.kurn")
	res, err := PublishBytes(dst, buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ForeignTemps) != 0 {
		t.Fatalf("foreign temps in a fresh dir: %v", res.ForeignTemps)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o444 {
		t.Fatalf("mode %v, want 0444", fi.Mode().Perm())
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, buf) {
		t.Fatal("published bytes differ")
	}
	if names := readDirNames(t, dir); !slices.Equal(names, []string{"list.kurn"}) {
		t.Fatalf("directory holds %v, want only the artifact", names)
	}
	if _, err := Verify(dst, LevelFull); err != nil {
		t.Fatal(err)
	}
}

func TestPublishRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "list.kurn")
	orig := []byte("someone else's file")
	if err := os.WriteFile(dst, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := PublishBytes(dst, artifactT(t, "ngram", 10))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	got, _ := os.ReadFile(dst)
	fi, _ := os.Stat(dst)
	if !bytes.Equal(got, orig) || fi.Mode().Perm() != 0o600 {
		t.Fatal("existing destination was modified")
	}
	if names := readDirNames(t, dir); !slices.Equal(names, []string{"list.kurn"}) {
		t.Fatalf("directory holds %v", names)
	}
	// A dangling symlink at the destination is also "existing".
	link := filepath.Join(dir, "dangling.kurn")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := PublishBytes(link, []byte("x")); !errors.Is(err, ErrExists) {
		t.Fatalf("dangling symlink: got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "nowhere")); !os.IsNotExist(err) {
		t.Fatal("publish wrote through a dangling symlink")
	}
}

// TestPublishLosesRaceWithoutDamage makes the destination appear between
// Publish's early check and its link: the link must fail, the winner's file
// must be untouched and the loser's temporary removed.
func TestPublishLosesRaceWithoutDamage(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "list.kurn")
	winner := []byte("winner")
	testHook = func(step string) error {
		if step == "chmod" {
			return os.WriteFile(dst, winner, 0o444)
		}
		return nil
	}
	defer func() { testHook = nil }()
	if _, err := PublishBytes(dst, artifactT(t, "exact", 5)); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, winner) {
		t.Fatal("winner's file was modified")
	}
	if names := readDirNames(t, dir); !slices.Equal(names, []string{"list.kurn"}) {
		t.Fatalf("loser left %v", names)
	}
}

func TestConcurrentPublishersExactlyOneWins(t *testing.T) {
	for round := 0; round < 20; round++ {
		dir := t.TempDir()
		dst := filepath.Join(dir, "list.kurn")
		const n = 8
		payloads := make([][]byte, n)
		for i := range payloads {
			payloads[i] = bytes.Repeat([]byte{byte('a' + i)}, 64<<10)
		}
		var wg sync.WaitGroup
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = PublishBytes(dst, payloads[i])
			}(i)
		}
		close(start)
		wg.Wait()
		wins := 0
		var winner int
		for i, err := range errs {
			switch {
			case err == nil:
				wins++
				winner = i
			case !errors.Is(err, ErrExists):
				t.Fatalf("publisher %d: %v", i, err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d publishers succeeded, want exactly 1", round, wins)
		}
		if got, _ := os.ReadFile(dst); !bytes.Equal(got, payloads[winner]) {
			t.Fatalf("round %d: destination does not hold the winner's complete bytes", round)
		}
		if names := readDirNames(t, dir); !slices.Equal(names, []string{"list.kurn"}) {
			t.Fatalf("round %d: leftovers %v", round, names)
		}
	}
}

// TestPublishInterruptedAtEveryStep simulates a crash after each step (no
// cleanup runs, as in a dead process), then checks what a crash there
// leaves and that a later publish reports but never removes the leftover.
func TestPublishInterruptedAtEveryStep(t *testing.T) {
	buf := artifactT(t, "ngram", 15)
	steps := []string{"created", "written", "synced", "closed", "chmod", "linked", "removed"}
	for _, crashAt := range steps {
		t.Run(crashAt, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "list.kurn")
			testHook = func(step string) error {
				if step == crashAt {
					return errSimulatedCrash
				}
				return nil
			}
			_, err := PublishBytes(dst, buf)
			testHook = nil
			if !errors.Is(err, errSimulatedCrash) {
				t.Fatalf("got %v", err)
			}
			linked := crashAt == "linked" || crashAt == "removed"
			// The final name is either absent or the complete artifact.
			if _, err := os.Lstat(dst); linked {
				if _, err := Verify(dst, LevelFull); err != nil {
					t.Fatalf("crash after link left an unverifiable artifact: %v", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("crash before link created the final name: %v", err)
			}
			leftovers, _ := findTemps(dir)
			if crashAt == "removed" {
				if len(leftovers) != 0 {
					t.Fatalf("temp survived its removal: %v", leftovers)
				}
				return
			}
			if len(leftovers) != 1 {
				t.Fatalf("crash left temps %v, want exactly one", leftovers)
			}
			before, _ := os.ReadFile(leftovers[0])

			// A later publish to another name reports the leftover and
			// leaves it byte-for-byte alone.
			res, err := PublishBytes(filepath.Join(dir, "other.kurn"), buf)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.ForeignTemps, leftovers) {
				t.Fatalf("reported %v, want %v", res.ForeignTemps, leftovers)
			}
			after, err := os.ReadFile(leftovers[0])
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("leftover was touched: %v", err)
			}
			// A retry of the same name succeeds exactly when the crash
			// came before the link.
			_, err = PublishBytes(dst, buf)
			if linked != errors.Is(err, ErrExists) || (!linked && err != nil) {
				t.Fatalf("retry after crash at %s: %v", crashAt, err)
			}
			if _, err := os.Stat(leftovers[0]); err != nil {
				t.Fatal("retry removed the leftover")
			}
		})
	}
}

func TestPublishWriteErrorRemovesOnlyOwnTemp(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, TempPrefix+"someoneelse"+TempSuffix)
	if err := os.WriteFile(foreign, []byte("live writer"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	res, err := Publish(filepath.Join(dir, "list.kurn"), func(w io.Writer) error {
		fmt.Fprint(w, "partial")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if !slices.Equal(res.ForeignTemps, []string{foreign}) {
		t.Fatalf("reported %v", res.ForeignTemps)
	}
	if names := readDirNames(t, dir); !slices.Equal(names, []string{filepath.Base(foreign)}) {
		t.Fatalf("directory holds %v, want only the foreign temp", names)
	}
}
