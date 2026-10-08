package workspace

import "testing"

func TestRepoHost(t *testing.T) {
	for name, tc := range map[string]struct {
		repo    string
		want    string
		wantErr bool
	}{
		"plain":        {repo: "https://github.com/oldsj/testrepo", want: "github.com"},
		"git suffix":   {repo: "https://GitHub.com/oldsj/testrepo.git", want: "github.com"},
		"http":         {repo: "http://github.com/o/r", wantErr: true},
		"ssh":          {repo: "git@github.com:o/r.git", wantErr: true},
		"credentials":  {repo: "https://x-access-token:secret@github.com/o/r", wantErr: true},
		"port":         {repo: "https://github.com:8443/o/r", wantErr: true},
		"query":        {repo: "https://github.com/o/r?x=1", wantErr: true},
		"fragment":     {repo: "https://github.com/o/r#x", wantErr: true},
		"no path":      {repo: "https://github.com", wantErr: true},
		"ip":           {repo: "https://10.0.0.1/o/r", wantErr: true},
		"single label": {repo: "https://localhost/o/r", wantErr: true},
		"empty":        {repo: "", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RepoHost(tc.repo)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("RepoHost(%q) = %q, %v; want %q, error=%v", tc.repo, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestGit(t *testing.T) {
	g := Git{Origins: []string{"github.com"}}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	if !g.Allows("GitHub.com.") || g.Allows("gitlab.com") {
		t.Fatal("Allows must match origins case-insensitively and nothing else")
	}
	for _, bad := range []Git{{}, {Origins: []string{"https://github.com"}}, {Origins: []string{"10.0.0.1"}}} {
		if bad.Validate() == nil {
			t.Fatalf("%v must be invalid", bad)
		}
	}
}

func TestGitCredentialRequiresOneOrigin(t *testing.T) {
	if err := (Git{Origins: []string{"github.com"}, Credential: true}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (Git{Origins: []string{"github.com", "gitlab.com"}, Credential: true}).Validate() == nil {
		t.Fatal("a credential is bound to one host and header, so two origins must be rejected")
	}
	if err := (Git{Origins: []string{"github.com", "gitlab.com"}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProxyPolicyAndTransport(t *testing.T) {
	good := Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(ReadProxyOrigin), PushProxyOrigin: new(PushProxyOrigin)}
	for _, repo := range []string{"https://github.com/Owner/Repo", "https://GitHub.com/Owner/Repo.git"} {
		read, push, err := good.Transport(repo)
		if err != nil || read != ReadProxyOrigin+"/owner/repo.git" || push != PushProxyOrigin+"/owner/repo.git" {
			t.Fatalf("transport = %q, %q, %v", read, push, err)
		}
	}
	readonly := good
	readonly.PushProxyOrigin = nil
	read, push, err := readonly.Transport("https://github.com/o/r")
	if err != nil || read != push {
		t.Fatalf("readonly transport = %q, %q, %v", read, push, err)
	}
	for _, repo := range []string{
		"https://github.com/o", "https://github.com/o/r/extra", "https://github.com/o/r/", "https://github.com//r", "https://github.com/o/..",
		"https://github.com/o/%2fr", "https://github.com/o/%72", "https://github.com/o/r%2e%2e", "https://github.com/o/r?x", "https://github.com/o/r#",
		"https://github.com./o/r", "https://github.com:443/o/r", "http://github.com/o/r", "https://user@github.com/o/r",
		"https://mainloop-git-read.mainloop.svc.cluster.local/o/r", "https://github.com/o/r\\extra",
	} {
		t.Run(repo, func(t *testing.T) {
			if _, _, err := good.Transport(repo); err == nil {
				t.Fatal("accepted invalid proxy identity")
			}
		})
	}
	for name, mutate := range map[string]func(*Git){
		"push without read":   func(g *Git) { g.ReadProxyOrigin = nil },
		"legacy credential":   func(g *Git) { g.Credential = true },
		"other identity":      func(g *Git) { g.Origins = []string{"gitlab.com"} },
		"multiple identities": func(g *Git) { g.Origins = []string{"github.com", "gitlab.com"} },
		"empty read":          func(g *Git) { g.ReadProxyOrigin = new("") },
	} {
		t.Run(name, func(t *testing.T) {
			g := good
			mutate(&g)
			if err := g.Validate(); err == nil {
				t.Fatal("accepted invalid policy")
			}
		})
	}
	for _, bad := range []string{ReadProxyOrigin + "/", ReadProxyOrigin + ":80", ReadProxyOrigin + ".", ReadProxyOrigin + "?", ReadProxyOrigin + "#", "http://MAINLOOP-git-read.mainloop.svc.cluster.local", "http://127.0.0.1"} {
		g := good
		g.ReadProxyOrigin = new(bad)
		if err := g.Validate(); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	legacy := Git{Origins: []string{"gitlab.com"}, Credential: true}
	read, push, err = legacy.Transport("https://gitlab.com/group/subgroup/repo.git")
	if err != nil || read != "https://gitlab.com/group/subgroup/repo.git" || push != "" {
		t.Fatalf("legacy = %q %q %v", read, push, err)
	}
}
