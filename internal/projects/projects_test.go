package projects

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// tree makes folders under a new root; a name ending in "/.git" makes that
// folder a repository.
func tree(t *testing.T, dirs ...string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func rel(t *testing.T, root string, dirs []string) []string {
	t.Helper()
	out := []string{}
	for _, d := range dirs {
		out = append(out, Shown(root, d))
	}
	return out
}

func TestFindListsRepositoriesAtAnyLevel(t *testing.T) {
	root := tree(t,
		"top/.git",
		"client/shop/.git",
		"client/shop/app/.git", // a repository inside a repository
		"client/group/deep/site/.git",
		"client/notes", // no repository
	)
	got, err := Find(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"client/group/deep/site", "client/shop", "client/shop/app", "top"}
	if g := rel(t, root, got); !reflect.DeepEqual(g, want) {
		t.Fatalf("found %q, want %q", g, want)
	}
}

// A linked worktree or a submodule has a .git file, not a folder.
func TestFindTakesAGitFile(t *testing.T) {
	root := tree(t, "linked")
	if err := os.WriteFile(filepath.Join(root, "linked", ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Find(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if g := rel(t, root, got); !reflect.DeepEqual(g, []string{"linked"}) {
		t.Fatalf("found %q", g)
	}
}

func TestFindSkipsWhatIsNotAProject(t *testing.T) {
	root := tree(t,
		"shop/.git",
		"shop/node_modules/dep/.git",
		"shop/vendor/dep/.git",
		"shop/.claude/worktrees/fix/.git",
		".hidden/repo/.git",
		"a/b/c/d/e/.git", // five levels down
	)
	got, err := Find(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if g := rel(t, root, got); !reflect.DeepEqual(g, []string{"shop"}) {
		t.Fatalf("found %q", g)
	}
}

// A link can point anywhere: following it would offer folders outside the
// root.
func TestFindDoesNotFollowLinks(t *testing.T) {
	root := tree(t, "shop/.git")
	outside := tree(t, "secret/.git")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := Find(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if g := rel(t, root, got); !reflect.DeepEqual(g, []string{"shop"}) {
		t.Fatalf("found %q", g)
	}
}

func TestFindMissingRoot(t *testing.T) {
	if _, err := Find(context.Background(), filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("a missing root must be an error")
	}
}

func TestFindStopsWithContext(t *testing.T) {
	root := tree(t, "shop/.git")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Find(ctx, root); err == nil {
		t.Fatal("a cancelled search must be an error")
	}
}

func TestHomeFoldsAWorktreeIntoItsProject(t *testing.T) {
	for dir, want := range map[string]string{
		"/code/shop/.claude/worktrees/fix":     "/code/shop",
		"/code/shop/.claude/worktrees/fix/sub": "/code/shop",
		"/code/shop":                           "/code/shop",
		"/code/shop/.claude":                   "/code/shop/.claude",
	} {
		if got := Home(dir); got != want {
			t.Errorf("Home(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestMatch(t *testing.T) {
	root := "/code"
	dirs := []string{"/code/client/shop", "/code/client/shop-admin", "/code/old/workshop", "/code/acme/blog", "/code/shop-tools/api"}
	for _, c := range []struct {
		name, query string
		want        []string
	}{
		{"a whole name wins over longer ones", "shop", []string{"client/shop"}},
		{"case does not matter", "SHOP-ADMIN", []string{"client/shop-admin"}},
		{"start of a name before inside of one", "sho", []string{"client/shop", "client/shop-admin", "old/workshop", "shop-tools/api"}},
		{"the group counts", "acme", []string{"acme/blog"}},
		{"every word must match", "client admin", []string{"client/shop-admin"}},
		{"no match", "nothing", []string{}},
		{"only spaces match nothing", "   ", []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := rel(t, root, Match(dirs, root, c.query)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Match(%q) = %q, want %q", c.query, got, c.want)
			}
		})
	}
}

// Two projects with the same folder name are both offered.
func TestMatchSameNameTwice(t *testing.T) {
	root := "/code"
	dirs := []string{"/code/a/tap", "/code/b/tap", "/code/b/tap-tools"}
	want := []string{"a/tap", "b/tap"}
	if got := rel(t, root, Match(dirs, root, "tap")); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUnder(t *testing.T) {
	root := tree(t, "shop", "other")
	outside := tree(t, "secret")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dir  string
		want bool
	}{
		{filepath.Join(root, "shop"), true},
		{root, false}, // the root itself is no project
		{filepath.Join(root, "link"), false},
		{filepath.Join(root, "file"), false},
		{filepath.Join(root, "gone"), false},
		{filepath.Join(root, "shop", "..", "..", filepath.Base(outside)), false},
		{filepath.Join(outside, "secret"), false},
	} {
		if got := Under(root, c.dir); got != c.want {
			t.Errorf("Under(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}

// A root that is itself reached through a link still holds its projects.
func TestUnderLinkedRoot(t *testing.T) {
	real := tree(t, "shop")
	link := filepath.Join(t.TempDir(), "code")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !Under(link, filepath.Join(link, "shop")) {
		t.Fatal("a project under a linked root must count")
	}
}

func TestResolveGivesTheRealFolder(t *testing.T) {
	root := tree(t, "client/shop")
	if err := os.Symlink(filepath.Join(root, "client", "shop"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got, ok := Resolve(root, filepath.Join(root, "link"))
	if want := filepath.Join(root, "client", "shop"); !ok || got != want {
		t.Fatalf("Resolve = %q %v, want %q", got, ok, want)
	}
}
