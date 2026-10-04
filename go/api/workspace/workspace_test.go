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
