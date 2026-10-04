package workspace

import (
	"fmt"

	"github.com/kagent-dev/kagent/go/adk/pkg/controllerclient"
	"github.com/kagent-dev/kagent/go/adk/pkg/taskstore"
	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/core/pkg/env"
)

// OpenController dials the control plane and returns the channel together with
// a workspace Source over it. The caller owns the client and passes it to the
// A2A app so both share one connection.
func OpenController(appName string) (*controllerclient.Client, Source, error) {
	apiURL := env.KagentAPIURL.Get()
	if apiURL == "" {
		return nil, nil, fmt.Errorf("%s is required to read the workspace request", env.KagentAPIURL.Name())
	}
	client, err := controllerclient.New(controllerclient.Config{APIURL: apiURL, AgentName: appName})
	if err != nil {
		return nil, nil, err
	}
	return client, FromTaskStore(taskstore.New(client, apia2a.RuntimeIdentityPath)), nil
}
