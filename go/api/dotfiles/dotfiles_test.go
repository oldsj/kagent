package dotfiles

import (
	"errors"
	"testing"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

func TestCredentialIsTheOneBoundToTheURLOrigin(t *testing.T) {
	credentials := []*apiv1.SessionCredential{
		{Origin: "https://api.example.test", Header: "authorization"},
		{Origin: "http://mcp.example.test:8000", Header: "x-mcp-token"},
	}
	c, err := Credential(&apiv1.Dotfiles{Url: "http://MCP.example.test:8000/dotfiles/x"}, credentials)
	if err != nil || c.GetHeader() != "x-mcp-token" {
		t.Fatalf("Credential = %v, %v", c, err)
	}
	for _, url := range []string{
		"http://mcp.example.test/dotfiles/x",       // another port
		"https://mcp.example.test:8000/dotfiles/x", // another scheme
		"http://other.example.test:8000/dotfiles/x",
	} {
		if _, err := Credential(&apiv1.Dotfiles{Url: url}, credentials); !errors.Is(err, ErrNoCredential) {
			t.Errorf("%s: %v, want no credential", url, err)
		}
	}
	for _, url := range []string{"ftp://mcp.example.test/x", "http://u:p@mcp.example.test:8000/x", "http://mcp.example.test:8000/x?a=1", "http://mcp.example.test:8000/x#f", "/dotfiles/x", ""} {
		if _, err := Origin(url); err == nil {
			t.Errorf("%q: accepted", url)
		}
	}
}
