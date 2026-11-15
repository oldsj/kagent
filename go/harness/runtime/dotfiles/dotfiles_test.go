package dotfiles

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// Fictional dotfiles; nothing here comes from a real dotfiles repository.
var sample = []File{
	{Path: "AGENTS.md", Data: []byte("# Owner notes\nPrefer small changes.\n")},
	{Path: "agents/reviewer.md", Data: []byte("---\nname: reviewer\n---\nReview.\n")},
	{Path: "commands/ship.md", Data: []byte("Ship it.\n")},
	{Path: "skills/release/SKILL.md", Data: []byte("---\nname: release\n---\nRelease.\n")},
	{Path: "skills/release/scripts/tag.sh", Executable: true, Data: []byte("#!/bin/sh\necho tag\n")},
}

// dataDir is a temporary data directory whose read-only layout is removable.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { _ = removeEntry(dir) })
	return dir
}

func bundle(t *testing.T, files []File) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, f := range files {
		mode := int64(0o444)
		if f.Executable {
			mode = 0o555
		}
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.Path, Mode: mode, Size: int64(len(f.Data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeSource struct {
	delivery *runtimev1.DotfilesDelivery
	err      error
}

func (f fakeSource) Dotfiles(context.Context) (*runtimev1.DotfilesDelivery, error) {
	return f.delivery, f.err
}

type fakeRunner struct{ turns int }

func (f *fakeRunner) Run(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error) {
	f.turns++
	return runtime.Outcome{}, nil
}

// server serves body at /dotfiles/<digest> and records the header it was asked with.
type server struct {
	*httptest.Server
	hits   atomic.Int32
	header atomic.Value
}

func serve(t *testing.T, body []byte) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.header.Store(r.Header.Get("X-Mcp-Token"))
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func delivery(s *server, body []byte) *runtimev1.DotfilesDelivery {
	return &runtimev1.DotfilesDelivery{Url: s.URL + "/dotfiles/x", Digest: Digest(body), Header: "x-mcp-token"}
}

func turn(t *testing.T, l *Layer) {
	t.Helper()
	if _, err := l.Run(context.Background(), runtime.Turn{}, nil); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestClaudeLaysOutEveryFileReadOnlyBeforeTheTurn(t *testing.T) {
	body := bundle(t, sample)
	srv := serve(t, body)
	data := dataDir(t)
	next := &fakeRunner{}
	l := New(next, fakeSource{delivery: delivery(srv, body)}, Claude(filepath.Join(data, "claude")), filepath.Join(data, ".kagent", "dotfiles"))
	turn(t, l)

	dir := filepath.Join(data, "claude")
	if next.turns != 1 {
		t.Fatalf("turns = %d", next.turns)
	}
	if got := srv.header.Load(); got != placeholder {
		t.Errorf("credential header = %q, want the placeholder the gateway overwrites", got)
	}
	for _, f := range sample {
		if got := read(t, filepath.Join(dir, f.Path)); got != string(f.Data) {
			t.Errorf("%s = %q", f.Path, got)
		}
	}
	if got := read(t, filepath.Join(dir, "CLAUDE.md")); got != "@AGENTS.md\n" {
		t.Errorf("generated CLAUDE.md = %q", got)
	}
	for path, want := range map[string]os.FileMode{
		"AGENTS.md": 0o444, "skills/release/SKILL.md": 0o444, "skills/release/scripts/tag.sh": 0o555,
		"skills/release": 0o555, "skills/release/scripts": 0o555, "agents/reviewer.md": 0o444,
	} {
		if got := mode(t, filepath.Join(dir, path)); got != want {
			t.Errorf("mode of %s = %v, want %v", path, got, want)
		}
	}
	// The root and the shared parents stay the CLI's.
	for _, path := range []string{"", "skills", "agents"} {
		if got := mode(t, filepath.Join(dir, path)); got&0o200 == 0 {
			t.Errorf("%q is not writable: %v", path, got)
		}
	}
}

func TestABundleClaudeMDIsUsedAsIs(t *testing.T) {
	files := append([]File{{Path: "CLAUDE.md", Data: []byte("Own Claude notes.\n")}}, sample...)
	body := bundle(t, files)
	data := dataDir(t)
	l := New(&fakeRunner{}, fakeSource{delivery: delivery(serve(t, body), body)}, Claude(filepath.Join(data, "claude")), filepath.Join(data, "state"))
	turn(t, l)
	if got := read(t, filepath.Join(data, "claude", "CLAUDE.md")); got != "Own Claude notes.\n" {
		t.Fatalf("CLAUDE.md = %q", got)
	}
}

func TestCodexTakesInstructionsAndSkillsAndYieldsToCompilerSkills(t *testing.T) {
	files := append([]File{{Path: "skills/lint/SKILL.md", Data: []byte("dotfiles lint\n")}}, sample...)
	body := bundle(t, files)
	data := dataDir(t)
	codex := filepath.Join(data, "codex")
	if err := os.MkdirAll(filepath.Join(codex, "skills", "lint"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codex, "skills", "lint", "SKILL.md"), []byte("compiler lint\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(&fakeRunner{}, fakeSource{delivery: delivery(serve(t, body), body)}, Codex(codex), filepath.Join(data, "state"))
	turn(t, l)

	if got := read(t, filepath.Join(codex, "AGENTS.md")); got != string(sample[0].Data) {
		t.Errorf("AGENTS.md = %q", got)
	}
	if got := read(t, filepath.Join(codex, "skills", "release", "SKILL.md")); got != string(sample[3].Data) {
		t.Errorf("release skill = %q", got)
	}
	if got := read(t, filepath.Join(codex, "skills", "lint", "SKILL.md")); got != "compiler lint\n" {
		t.Errorf("the compiler's skill lost the clash: %q", got)
	}
	for _, path := range []string{"CLAUDE.md", "agents/reviewer.md", "commands/ship.md"} {
		if _, err := os.Lstat(filepath.Join(codex, path)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was laid out for Codex: %v", path, err)
		}
	}
}

// Codex setup re-materializes its skills root; the next turn lays the dotfiles
// skills out again, from the verified copy, without another download.
func TestARemovedEntryIsLaidOutAgainBeforeTheNextTurn(t *testing.T) {
	body := bundle(t, sample)
	srv := serve(t, body)
	data := dataDir(t)
	codex := filepath.Join(data, "codex")
	l := New(&fakeRunner{}, fakeSource{delivery: delivery(srv, body)}, Codex(codex), filepath.Join(data, "state"))
	turn(t, l)
	if err := removeEntry(filepath.Join(codex, "skills")); err != nil {
		t.Fatal(err)
	}
	turn(t, l)
	if got := read(t, filepath.Join(codex, "skills", "release", "SKILL.md")); got != string(sample[3].Data) {
		t.Fatalf("release skill = %q", got)
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("downloads = %d, want 1", n)
	}
}

// A new process lays the pinned files out again, so an agent's edits and
// additions to them do not survive a restart; the rest of the directory does.
func TestARestartRewritesTheDotfilesAndKeepsTheRest(t *testing.T) {
	body := bundle(t, sample)
	srv := serve(t, body)
	data := dataDir(t)
	dir := filepath.Join(data, "claude")
	state := filepath.Join(data, "state")
	turn(t, New(&fakeRunner{}, fakeSource{delivery: delivery(srv, body)}, Claude(dir), state))

	agents := filepath.Join(dir, "AGENTS.md")
	if err := os.Chmod(agents, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agents, []byte("edited by the agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "skills", "release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "release", "extra.md"), []byte("added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(own, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	turn(t, New(&fakeRunner{}, fakeSource{delivery: delivery(srv, body)}, Claude(dir), state))
	if got := read(t, agents); got != string(sample[0].Data) {
		t.Errorf("AGENTS.md = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "skills", "release", "extra.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the agent's addition survived: %v", err)
	}
	if got := read(t, own); got != "{}" {
		t.Errorf("the CLI's own file was touched: %q", got)
	}
	if n := srv.hits.Load(); n != 1 {
		t.Errorf("downloads = %d, want 1: the restart uses the verified copy", n)
	}
}

// A fork carries its source's files and verified copy in its restored data
// and has no delivery of its own; it lays out the same bundle again.
func TestAForkLaysOutItsSourceBundleAgain(t *testing.T) {
	body := bundle(t, sample)
	data := dataDir(t)
	dir := filepath.Join(data, "claude")
	state := filepath.Join(data, "state")
	turn(t, New(&fakeRunner{}, fakeSource{delivery: delivery(serve(t, body), body)}, Claude(dir), state))
	if err := os.Chmod(filepath.Join(dir, "AGENTS.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn(t, New(&fakeRunner{}, fakeSource{}, Claude(dir), state))
	if got := read(t, filepath.Join(dir, "AGENTS.md")); got != string(sample[0].Data) {
		t.Fatalf("AGENTS.md = %q", got)
	}
}

func TestASessionWithoutDotfilesLaysOutNothing(t *testing.T) {
	data := dataDir(t)
	next := &fakeRunner{}
	l := New(next, fakeSource{}, Claude(filepath.Join(data, "claude")), filepath.Join(data, "state"))
	turn(t, l)
	turn(t, l)
	if next.turns != 2 {
		t.Fatalf("turns = %d", next.turns)
	}
	if entries, _ := os.ReadDir(filepath.Join(data, "claude")); len(entries) != 0 {
		t.Fatalf("laid out %v", entries)
	}
}

// Dotfiles are context: a bad bundle leaves them out, the turn still runs,
// and the next turn tries again.
func TestABundleThatFailsVerificationIsNotLaidOut(t *testing.T) {
	body := bundle(t, sample)
	srv := serve(t, body)
	data := dataDir(t)
	next := &fakeRunner{}
	d := delivery(srv, body)
	d.Digest = Digest([]byte("something else"))
	l := New(next, fakeSource{delivery: d}, Claude(filepath.Join(data, "claude")), filepath.Join(data, "state"))
	turn(t, l)
	turn(t, l)
	if next.turns != 2 {
		t.Fatalf("turns = %d", next.turns)
	}
	if _, err := os.Lstat(filepath.Join(data, "claude", "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unverified bundle was laid out: %v", err)
	}
	if n := srv.hits.Load(); n != 2 {
		t.Fatalf("downloads = %d, want a retry per turn", n)
	}
}

func TestTheCredentialIsNotFollowedThroughARedirect(t *testing.T) {
	body := bundle(t, sample)
	elsewhere := serve(t, body)
	redirect := httptest.NewServer(http.RedirectHandler(elsewhere.URL+"/dotfiles/x", http.StatusFound))
	t.Cleanup(redirect.Close)
	data := dataDir(t)
	d := &runtimev1.DotfilesDelivery{Url: redirect.URL + "/dotfiles/x", Digest: Digest(body), Header: "x-mcp-token"}
	turn(t, New(&fakeRunner{}, fakeSource{delivery: d}, Claude(filepath.Join(data, "claude")), filepath.Join(data, "state")))
	if n := elsewhere.hits.Load(); n != 0 {
		t.Fatalf("the redirect was followed")
	}
}

func TestUnpackRefusesWhatTheLayoutDoesNotTake(t *testing.T) {
	link := func() []byte {
		var buf bytes.Buffer
		w := tar.NewWriter(&buf)
		_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "AGENTS.md", Linkname: "/etc/passwd"})
		_ = w.Close()
		return buf.Bytes()
	}()
	big := bytes.Repeat([]byte("a"), 2<<20+1)
	for name, body := range map[string][]byte{
		"symlink":        link,
		"escape":         bundle(t, []File{{Path: "../AGENTS.md", Data: []byte("x")}}),
		"absolute":       bundle(t, []File{{Path: "/AGENTS.md", Data: []byte("x")}}),
		"unlisted":       bundle(t, []File{{Path: "settings.json", Data: []byte("{}")}}),
		"nested command": bundle(t, []File{{Path: "commands/a/b.md", Data: []byte("x")}}),
		"bare skill":     bundle(t, []File{{Path: "skills/x", Data: []byte("x")}}),
		"oversized file": bundle(t, []File{{Path: "AGENTS.md", Data: big}}),
		"duplicate":      bundle(t, []File{{Path: "AGENTS.md", Data: []byte("a")}, {Path: "AGENTS.md", Data: []byte("b")}}),
	} {
		if _, err := Unpack(body, Digest(body)); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
	body := bundle(t, sample)
	if _, err := Unpack(body, Digest(append(body, 0))); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("digest mismatch: %v", err)
	}
	files, err := Unpack(body, Digest(body))
	if err != nil || len(files) != len(sample) {
		t.Fatalf("Unpack = %d files, %v", len(files), err)
	}
}

// A symlink an agent puts where a shared parent belongs is replaced, so the
// layout never writes through it.
func TestTheLayoutDoesNotWriteThroughASymlinkedParent(t *testing.T) {
	body := bundle(t, sample)
	data := dataDir(t)
	dir := filepath.Join(data, "claude")
	outside := filepath.Join(data, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dir, "skills")); err != nil {
		t.Fatal(err)
	}
	turn(t, New(&fakeRunner{}, fakeSource{delivery: delivery(serve(t, body), body)}, Claude(dir), filepath.Join(data, "state")))
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote through the symlink: %v", entries)
	}
	if got := read(t, filepath.Join(dir, "skills", "release", "SKILL.md")); got != string(sample[3].Data) {
		t.Fatalf("release skill = %q", got)
	}
}
