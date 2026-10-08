// Copyright 2026 The kagent Authors
// SPDX-License-Identifier: Apache-2.0
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/api/workspace"
	kagentenv "github.com/kagent-dev/kagent/go/core/pkg/env"
	"github.com/kagent-dev/mockllm"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// This admission case requires a deliberately installed controller/CRD pair.
// It performs no Session send or upstream Git operation.
func TestGitProxyHarnessAdmission(t *testing.T) {
	_ = interactionTarget(t)
	kube := interactionKubeClient(t)
	for _, baseName := range []string{claudeE2EHarness, codexE2EHarness} {
		base := &v1alpha3.Harness{}
		require.NoError(t, kube.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "kagent", Name: baseName}, base))
		for _, tc := range []struct {
			name   string
			mutate func(*v1alpha3.HarnessGit)
			valid  bool
		}{
			{"read", func(g *v1alpha3.HarnessGit) { g.PushProxyOrigin = nil }, true},
			{"read-push", func(*v1alpha3.HarnessGit) {}, true},
			{"push-alone", func(g *v1alpha3.HarnessGit) { g.ReadProxyOrigin = nil }, false},
			{"direct-pat", func(g *v1alpha3.HarnessGit) {
				g.CredentialSecretRef = &v1alpha3.SecretKeyReference{Name: "mainloop-git-auth", Key: "authorization"}
			}, false},
			{"alternate-origin", func(g *v1alpha3.HarnessGit) { g.ReadProxyOrigin = new(workspace.ReadProxyOrigin + ":80") }, false},
			{"other-identity", func(g *v1alpha3.HarnessGit) { g.Origins = []string{"gitlab.com"} }, false},
		} {
			t.Run(baseName+"/"+tc.name, func(t *testing.T) {
				harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Namespace: "kagent", GenerateName: strings.TrimSuffix(baseName, "-e2e") + "-proxy-"}, Spec: *base.Spec.DeepCopy()}
				harness.Spec.Git = &v1alpha3.HarnessGit{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin), PushProxyOrigin: new(workspace.PushProxyOrigin)}
				tc.mutate(harness.Spec.Git)
				err := kube.Create(t.Context(), harness)
				if !tc.valid {
					require.True(t, apierrors.IsInvalid(err), "expected admission rejection: %v", err)
					return
				}
				require.NoError(t, err)
				t.Cleanup(func() {
					err := kube.Delete(context.Background(), harness)
					if !apierrors.IsNotFound(err) {
						require.NoError(t, err)
					}
				})
			})
		}
	}
}

type proxyE2EOrigin struct {
	server               *httptest.Server
	bare                 string
	reads, writes        atomic.Int64
	readValue, pushValue string
}

// The fixed-name Services are created only in a disposable E2E cluster. An
// occupied name fails creation; this fixture never replaces installed listeners.
func proxyE2EServices(t *testing.T, kube ctrlclient.Client, f *proxyE2EOrigin) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mainloop"}}
	err := kube.Create(t.Context(), ns)
	if err == nil {
		t.Cleanup(func() { require.NoError(t, kube.Delete(context.Background(), ns)) })
	} else {
		require.True(t, apierrors.IsAlreadyExists(err), "%v", err)
	}
	address := kagentenv.KagentLocalHost.Get()
	if address == "" {
		address = "172.17.0.1"
	}
	require.NotNil(t, net.ParseIP(address), "Git proxy fixture requires a cluster-reachable IP in KAGENT_E2E_LOCAL_HOST")
	parsed, err := url.Parse(f.server.URL)
	require.NoError(t, err)
	number, err := strconv.ParseInt(parsed.Port(), 10, 32)
	require.NoError(t, err)
	for _, name := range []string{"mainloop-git-read", "mainloop-git-push"} {
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "mainloop", Name: name}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}}
		require.NoError(t, kube.Create(t.Context(), service), "fixed proxy names must be unoccupied")
		t.Cleanup(func() { require.NoError(t, kube.Delete(context.Background(), service)) })
		addressType := discoveryv1.AddressTypeIPv4
		if net.ParseIP(address).To4() == nil {
			addressType = discoveryv1.AddressTypeIPv6
		}
		slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: service.Namespace, Name: service.Name, Labels: map[string]string{discoveryv1.LabelServiceName: service.Name, discoveryv1.LabelManagedBy: "kagent-e2e"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: service.Name, UID: service.UID}}},
			AddressType: addressType, Ports: []discoveryv1.EndpointPort{{Name: new("http"), Port: new(int32(number)), Protocol: new(corev1.ProtocolTCP)}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{address}, Conditions: discoveryv1.EndpointConditions{Ready: new(true)}}}}
		require.NoError(t, kube.Create(t.Context(), slice))
		t.Cleanup(func() {
			err := kube.Delete(context.Background(), slice)
			if !apierrors.IsNotFound(err) {
				require.NoError(t, err)
			}
		})
	}
}
func newProxyE2EOrigin(t *testing.T) *proxyE2EOrigin {
	t.Helper()
	f := &proxyE2EOrigin{bare: filepath.Join(t.TempDir(), "owner", "repo.git"), readValue: "Bearer read-fixture-" + uuid.NewString(), pushValue: "Bearer push-fixture-" + uuid.NewString()}
	work := t.TempDir()
	git, err := exec.LookPath("git")
	require.NoError(t, err)
	command := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	command(work, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(work, "README"), []byte("local Git fixture\n"), 0600))
	command(work, "add", ".")
	command(work, "commit", "-q", "-m", "fixture")
	require.NoError(t, os.MkdirAll(filepath.Dir(f.bare), 0700))
	command(work, "clone", "-q", "--bare", work, f.bare)
	command(f.bare, "config", "http.receivepack", "true")
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(filepath.Dir(f.bare)), "GIT_HTTP_EXPORT_ALL=1"}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.TrimSuffix(r.Host, ":80")
		read := host == "mainloop-git-read.mainloop.svc.cluster.local"
		push := host == "mainloop-git-push.mainloop.svc.cluster.local"
		receive := strings.Contains(r.URL.Path, "git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
		expected := f.readValue
		if push {
			expected = f.pushValue
		}
		if (!read && !push) || !strings.HasPrefix(r.URL.Path, "/owner/repo.git/") || r.Header.Get("Authorization") != expected || (read && receive) {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		if receive {
			f.writes.Add(1)
		} else {
			f.reads.Add(1)
		}
		backend.ServeHTTP(w, r)
	}))
	require.NoError(t, server.Listener.Close())
	server.Listener, err = net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	server.Start()
	t.Cleanup(server.Close)
	f.server = server
	return f
}

// Written for later installed qualification. This case uses real native Git,
// gateway lookup and local smart-HTTP upstream; no GitHub repository is contacted.
// Local source checks compile this case without running cluster or native calls.
func TestGitProxyDeferredValuesAndBootstrap(t *testing.T) {
	target := interactionTarget(t)
	kube := interactionKubeClient(t)
	origin := newProxyE2EOrigin(t)
	proxyE2EServices(t, kube, origin)
	raw, err := gitWorkspaceMocks.ReadFile("mocks/invoke_claude_git_workspace.json")
	require.NoError(t, err)
	probe := "cd /data/workspace; echo READ_URL=$(git remote get-url origin); echo PUSH_URL=$(git remote get-url --push origin); echo READ_HEADER=$(git config --get http." + workspace.ReadProxyOrigin + "/.extraHeader); echo PUSH_HEADER=$(git config --get http." + workspace.PushProxyOrigin + "/.extraHeader);"
	raw = bytes.ReplaceAll(raw, []byte("cd /data/workspace;"), []byte(probe))
	raw = bytes.ReplaceAll(raw, []byte("printf KEEP_ME >"), []byte("git config user.name fixture && git config user.email fixture@example.test && git commit --allow-empty -m fixture && git push origin HEAD:refs/heads/feature-e2e && printf KEEP_ME >"))
	var modelConfig mockllm.Config
	require.NoError(t, json.Unmarshal(raw, &modelConfig))
	model := startGatedModelProxy(t, startMockLLMConfig(t, modelConfig), "")
	modelDefinition := createClaudeMockModel(t, kube, reachableServerURL(t, model.URL, ""))
	base := &v1alpha3.Harness{}
	require.NoError(t, kube.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "kagent", Name: claudeE2EHarness}, base))
	harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{GenerateName: "git-proxy-", Namespace: "kagent"}, Spec: *base.Spec.DeepCopy()}
	harness.Spec.Git = &v1alpha3.HarnessGit{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin), PushProxyOrigin: new(workspace.PushProxyOrigin)}
	require.NoError(t, kube.Create(t.Context(), harness))
	t.Cleanup(func() { require.NoError(t, kube.Delete(context.Background(), harness)) })
	template := createGitWorkspaceTemplate(t, kube, harness.Name, modelDefinition.Name)
	conn := newControllerConn(t, target)
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(t.Context(), "x-user-id", "e2e"), 4*time.Minute)
	t.Cleanup(cancel)
	sessions := apiv1alpha1.NewSessionServiceClient(conn)
	readName, pushName := "mainloop-git-read-"+uuid.NewString(), "mainloop-git-push-"+uuid.NewString()
	request := &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: template}, RequestId: uuid.NewString(), Workspace: &apiv1alpha1.Workspace{Repo: "https://github.com/Owner/Repo", Ref: "main", Branch: "feature-e2e"}, Credentials: []*apiv1alpha1.SessionCredential{
		{Origin: workspace.ReadProxyOrigin, Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: readName, Key: "authorization"}},
		{Origin: workspace.PushProxyOrigin, Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: pushName, Key: "authorization"}},
	}}
	created, err := sessions.CreateSession(ctx, request)
	require.NoError(t, err)
	fixture := &interactionFixture{ctx: ctx, sessions: sessions, client: a2apb.NewA2AServiceClient(conn), sessionID: created.Session.Id, contextID: created.Session.ContextId, tenant: "kagent/" + template}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "x-user-id", "e2e"), time.Minute)
		defer cleanupCancel()
		require.NoError(t, deleteIdleSession(cleanupCtx, sessions, fixture.sessionID))
	})
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, created.Session.State)
	require.Zero(t, origin.reads.Load())
	require.Zero(t, model.countContaining("WS_PROBE_ONE"))
	before, err := sessions.GetSession(ctx, &apiv1alpha1.GetSessionRequest{SessionId: fixture.sessionID})
	require.NoError(t, err)
	require.Nil(t, before.Session.RuntimeAssociation)
	warm := func() *apiv1alpha1.RuntimeAssociation {
		t.Helper()
		suspendWhenSettled(t, fixture)
		_, err := sessions.ResumeSession(ctx, &apiv1alpha1.ResumeSessionRequest{SessionId: fixture.sessionID})
		require.NoError(t, err)
		var association *apiv1alpha1.RuntimeAssociation
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
			get, err := sessions.GetSession(ctx, &apiv1alpha1.GetSessionRequest{SessionId: fixture.sessionID})
			if err != nil {
				return false, err
			}
			association = get.Session.RuntimeAssociation
			return association != nil && association.CurrentActive, nil
		}))
		return association
	}
	association := warm()
	require.NotEmpty(t, association.ActorUid)
	require.Equal(t, "active", association.Phase)
	require.Zero(t, origin.reads.Load())
	// Intentional missing-value negative: failed bootstrap must never reach model.
	failed := sendStreaming(t, fixture, "WS_PROBE_ONE: report the workspace state.")
	require.Equal(t, a2atype.TaskStateFailed, failed.state)
	require.Zero(t, model.countContaining("WS_PROBE_ONE"))
	require.Zero(t, origin.reads.Load())
	current := warm()
	require.True(t, proto.Equal(association, current), "retry uses the original runtime generation")
	// Publish only after a freshly observed exact association, preserving names.
	for name, value := range map[string]string{readName: origin.readValue, pushName: origin.pushValue} {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kagent", Name: name, Labels: map[string]string{"mainloop.dev/actor-egress": "true"}}, Immutable: new(true), StringData: map[string]string{"authorization": value}}
		require.NoError(t, kube.Create(ctx, secret))
		t.Cleanup(func() { require.NoError(t, kube.Delete(context.Background(), secret)) })
	}
	probeResult := probeWorkspace(t, fixture, "WS_PROBE_ONE: report the workspace state.", "toolu_ws_probe_one")
	require.Equal(t, "yes", probeResult.value("DONE"))
	require.Equal(t, "no", probeResult.value("STARTED"))
	require.Positive(t, origin.reads.Load())
	require.Equal(t, workspace.ReadProxyOrigin+"/owner/repo.git", probeResult.value("READ_URL"))
	require.Equal(t, workspace.PushProxyOrigin+"/owner/repo.git", probeResult.value("PUSH_URL"))
	require.Equal(t, "Authorization: Basic cGxhY2Vob2xkZXI=", probeResult.value("READ_HEADER"))
	require.Equal(t, probeResult.value("READ_HEADER"), probeResult.value("PUSH_HEADER"))
	require.Contains(t, probeResult.doneBody, "repo="+request.Workspace.Repo)
	written := sendStreaming(t, fixture, "WS_WRITE_FILE: write a note and a sentinel.")
	require.Equal(t, a2atype.TaskStateCompleted, written.state)
	require.Contains(t, taskToolResults(getTask(t, fixture, written.taskID))["toolu_ws_write"], "WRITTEN")
	require.Positive(t, origin.writes.Load())
	stored, err := sessions.GetSession(ctx, &apiv1alpha1.GetSessionRequest{SessionId: fixture.sessionID})
	require.NoError(t, err)
	require.True(t, proto.Equal(request.Workspace, stored.Session.Workspace))
	require.Len(t, stored.Session.Credentials, len(request.Credentials))
	for i, ref := range request.Credentials {
		require.True(t, proto.Equal(ref, stored.Session.Credentials[i]))
	}
}
