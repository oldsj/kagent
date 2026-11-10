package e2e_test

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/api/workspace"
	kagentenv "github.com/kagent-dev/kagent/go/core/pkg/env"
)

//go:embed mocks/invoke_claude_git_instance.json
var gitInstanceMocks embed.FS

// gitInstanceAgent is fixed because the installed session policy must list it.
const gitInstanceAgent = "git-instance-e2e"

// TestE2EGitInstanceProxy runs a Claude Session whose Harness routes Git
// through a non-default Mainloop Git instance. The instance's read and push
// listeners are a git http-backend on the test host, reached through Services
// in the instance namespace. The checkout and push must reach only that
// instance's listeners, each with its own per-Session credential injected by
// the egress gateway. A Harness pairing two instances and a Session binding the
// instance's Secret to another instance's listener are refused.
func TestE2EGitInstanceProxy(t *testing.T) {
	namespace := kagentenv.E2EGitInstance.Get()
	if namespace == "" {
		t.Skip("KAGENT_E2E_GIT_INSTANCE is not set")
	}
	target := interactionTarget(t)
	kube := interactionKubeClient(t)
	readOrigin, pushOrigin := workspace.GitInstanceOrigins(namespace)

	readSecret, readValue := createGitInstanceSecret(t, kube, namespace+"-git-read-")
	pushSecret, pushValue := createGitInstanceSecret(t, kube, namespace+"-git-push-")
	proxy := startGitInstanceProxy(t, namespace, readValue, pushValue)
	exposeGitInstanceProxy(t, kube, namespace, proxy.port)

	t.Run("a Harness pairing two instances is refused", func(t *testing.T) {
		mainloopRead, _ := workspace.GitInstanceOrigins("mainloop")
		harness := gitInstanceHarness(t, kube, mainloopRead, pushOrigin)
		err := kube.Create(t.Context(), harness)
		if err == nil {
			_ = kube.Delete(context.Background(), harness)
		}
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "same Mainloop Git instance") {
			t.Fatalf("create cross-instance Harness: %v, want the same-instance rule", err)
		}
	})

	harness := gitInstanceHarness(t, kube, readOrigin, pushOrigin)
	if err := kube.Create(t.Context(), harness); err != nil {
		t.Fatalf("create Git instance Harness: %v", err)
	}
	t.Cleanup(func() {
		if err := kube.Delete(context.Background(), harness); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete Git instance Harness: %v", err)
		}
	})
	modelURL := reachableServerURL(t, startMockLLMServer(t, gitInstanceMocks, "mocks/invoke_claude_git_instance.json"), "")
	model := createClaudeMockModel(t, kube, modelURL)
	template := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: gitInstanceAgent, Namespace: "kagent", Labels: map[string]string{"kagent.dev/e2e-runtime": "claude"}},
		Spec: v1alpha3.AgentTemplateSpec{
			ModelConfig: &v1alpha3.ObjectReference{Name: model.Name},
			Description: "Git instance proxy fixture", SystemPrompt: "Run the requested command.",
		},
	}
	createAndWaitInteractionTemplateForHarness(t, kube, template, harness.Name)

	credential := func(origin, secret string) *apiv1.SessionCredential {
		return &apiv1.SessionCredential{Origin: origin, Header: "authorization", SecretRef: &apiv1.SecretKeyReference{Name: secret, Key: "authorization"}}
	}
	request := func() *apiv1.CreateSessionRequest {
		return &apiv1.CreateSessionRequest{
			Agent: &apiv1.ResourceReference{Namespace: "kagent", Name: gitInstanceAgent}, RequestId: uuid.NewString(),
			Workspace:   &apiv1.Workspace{Repo: "https://github.com/owner/repo", Ref: proxy.sha, Branch: "feature"},
			Credentials: []*apiv1.SessionCredential{credential(readOrigin, readSecret), credential(pushOrigin, pushSecret)},
		}
	}

	t.Run("an instance's Secret bound to another instance's listener is refused", func(t *testing.T) {
		mainloopRead, _ := workspace.GitInstanceOrigins("mainloop")
		crossed := request()
		crossed.Credentials = []*apiv1.SessionCredential{credential(mainloopRead, readSecret)}
		ctx := metadata.AppendToOutgoingContext(t.Context(), "x-user-id", "e2e")
		_, err := apiv1.NewSessionServiceClient(newControllerConn(t, target)).CreateSession(ctx, crossed)
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("CreateSession with a crossed credential = %v, want PermissionDenied", err)
		}
	})

	fixture := newInteractionFixtureForRequest(t, target, request())
	streamed := sendStreaming(t, fixture, "Publish a commit through the Git instance.")
	if streamed.state != a2atype.TaskStateCompleted || !strings.Contains(streamed.text, "GIT_INSTANCE_DONE") {
		t.Logf("Git proxy requests: %+v", proxy.snapshot())
		t.Fatalf("Git instance task state = %s, text = %q, failure = %q", streamed.state, streamed.text, streamed.failureText)
	}
	assertToolEvents(t, streamed.toolEvents, "Bash")

	requests := proxy.snapshot()
	var fetched, received bool
	for _, r := range requests {
		switch {
		case r.denied != "":
			t.Errorf("proxy denied %s %s on %s: %s", r.method, r.path, r.host, r.denied)
		case r.host == proxy.readHost && strings.Contains(r.path+"?"+r.query, "git-upload-pack"):
			fetched = true
		case r.host == proxy.pushHost && strings.Contains(r.path+"?"+r.query, "git-receive-pack"):
			received = true
		}
	}
	if !fetched || !received {
		t.Fatalf("proxy requests = %+v; want an upload-pack on %s and a receive-pack on %s", requests, proxy.readHost, proxy.pushHost)
	}
	pushed := strings.TrimSpace(gitOutput(t, proxy.bare, "rev-parse", "refs/heads/git-instance-e2e^"))
	if pushed != proxy.sha {
		t.Fatalf("pushed commit parent = %s, want the checked-out %s", pushed, proxy.sha)
	}
}

func gitInstanceHarness(t *testing.T, kube ctrlclient.Client, readOrigin, pushOrigin string) *v1alpha3.Harness {
	t.Helper()
	base := &v1alpha3.Harness{}
	if err := kube.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "kagent", Name: claudeE2EHarness}, base); err != nil {
		t.Fatalf("read %s Harness: %v", claudeE2EHarness, err)
	}
	spec := *base.Spec.DeepCopy()
	// The runtime checkout must accept the instance, so a cluster whose shared
	// fixture predates it can name a current Claude Harness image.
	if image := kagentenv.E2EClaudeImage.Get(); image != "" {
		spec.Workload.Image = &image
	}
	spec.Git = &v1alpha3.HarnessGit{Origins: []string{"github.com"}, ReadProxyOrigin: &readOrigin, PushProxyOrigin: &pushOrigin}
	return &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{GenerateName: "claude-git-instance-", Namespace: "kagent"}, Spec: spec}
}

// createGitInstanceSecret creates a Secret matching the instance's Secret name
// pattern and returns its name and complete Authorization value.
func createGitInstanceSecret(t *testing.T, kube ctrlclient.Client, prefix string) (string, string) {
	t.Helper()
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	value := "Bearer " + hex.EncodeToString(token)
	// Mainloop labels its Git Secrets for actor egress; a credential provider
	// grant that selects Secrets by label resolves only labelled ones.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix, Namespace: "kagent", Labels: map[string]string{"mainloop.dev/actor-egress": "true"}},
		Data:       map[string][]byte{"authorization": []byte(value)},
	}
	if err := kube.Create(t.Context(), secret); err != nil {
		t.Fatalf("create Git instance Secret: %v", err)
	}
	t.Cleanup(func() {
		if err := kube.Delete(context.Background(), secret); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete Git instance Secret: %v", err)
		}
	})
	return secret.Name, value
}

type gitProxyRequest struct{ host, method, path, query, denied string }

type gitInstanceProxy struct {
	port               int
	bare, sha          string
	readHost, pushHost string
	mu                 sync.Mutex
	requests           []gitProxyRequest
}

func (p *gitInstanceProxy) snapshot() []gitProxyRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]gitProxyRequest(nil), p.requests...)
}

// startGitInstanceProxy serves owner/repo through git http-backend as both of
// an instance's listeners. Each listener requires its own injected credential,
// and the read listener refuses receive-pack.
func startGitInstanceProxy(t *testing.T, namespace, readAuthorization, pushAuthorization string) *gitInstanceProxy {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	gitOutput(t, root, "init", "-q", "--bare", "-b", "main", bare)
	gitOutput(t, bare, "config", "http.receivepack", "true")
	work := filepath.Join(root, "work")
	gitOutput(t, root, "init", "-q", "-b", "main", work)
	gitOutput(t, work, "-c", "user.name=kagent-e2e", "-c", "user.email=e2e@kagent.invalid", "commit", "--allow-empty", "-q", "-m", "initial")
	gitOutput(t, work, "push", "-q", bare, "main")
	readOrigin, pushOrigin := workspace.GitInstanceOrigins(namespace)
	p := &gitInstanceProxy{
		bare: bare, sha: strings.TrimSpace(gitOutput(t, bare, "rev-parse", "main")),
		readHost: strings.TrimPrefix(readOrigin, "http://"), pushHost: strings.TrimPrefix(pushOrigin, "http://"),
	}
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		record := gitProxyRequest{host: host, method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		receive := strings.Contains(r.URL.Path, "git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
		switch {
		case host != p.readHost && host != p.pushHost:
			record.denied = "foreign listener"
		case !strings.HasPrefix(r.URL.Path, "/owner/repo.git/"):
			record.denied = "foreign repository"
		case host == p.readHost && r.Header.Get("Authorization") != readAuthorization:
			record.denied = "missing read credential"
		case host == p.pushHost && r.Header.Get("Authorization") != pushAuthorization:
			record.denied = "missing push credential"
		case receive && host == p.readHost:
			record.denied = "read listener refuses writes"
		}
		p.mu.Lock()
		p.requests = append(p.requests, record)
		p.mu.Unlock()
		if record.denied != "" {
			http.Error(w, record.denied, http.StatusForbidden)
			return
		}
		r.URL.Path = "/origin.git/" + strings.TrimPrefix(r.URL.Path, "/owner/repo.git/")
		r.RequestURI = r.URL.RequestURI()
		backend.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	p.port = listener.Addr().(*net.TCPAddr).Port
	return p
}

// exposeGitInstanceProxy publishes the host proxy as the instance's read and
// push Services on port 80. The instance namespace is created if absent and
// then removed; existing listener Services are never replaced.
func exposeGitInstanceProxy(t *testing.T, kube ctrlclient.Client, namespace string, port int) {
	t.Helper()
	address := kagentenv.KagentLocalHost.Get()
	if address == "" && goruntime.GOOS == "linux" {
		address = "172.17.0.1"
	}
	if net.ParseIP(address) == nil {
		t.Fatalf("KAGENT_E2E_LOCAL_HOST must be the test host's IP as seen from the cluster, got %q (on Docker Desktop, resolve host.docker.internal inside a node)", address)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := kube.Create(t.Context(), ns); err == nil {
		t.Cleanup(func() {
			if err := kube.Delete(context.Background(), ns); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("delete Git instance namespace: %v", err)
			}
		})
	} else if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create Git instance namespace: %v", err)
	}
	addressType := discoveryv1.AddressTypeIPv4
	if net.ParseIP(address).To4() == nil {
		addressType = discoveryv1.AddressTypeIPv6
	}
	hostPort := int32(port) //nolint:gosec // A TCP listener port fits in int32.
	for _, name := range []string{"mainloop-git-read", "mainloop-git-push"} {
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "kagent-e2e"}},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(hostPort)}}},
		}
		if err := kube.Create(t.Context(), service); err != nil {
			t.Fatalf("create %s/%s Service: %v", namespace, name, err)
		}
		t.Cleanup(func() {
			if err := kube.Delete(context.Background(), service); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("delete %s/%s Service: %v", namespace, name, err)
			}
		})
		endpoints := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace,
				Labels:          map[string]string{discoveryv1.LabelServiceName: name, discoveryv1.LabelManagedBy: "kagent-e2e"},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: name, UID: service.UID}},
			},
			AddressType: addressType,
			Ports:       []discoveryv1.EndpointPort{{Name: new("http"), Port: new(hostPort), Protocol: new(corev1.ProtocolTCP)}},
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{address}, Conditions: discoveryv1.EndpointConditions{Ready: new(true)}}},
		}
		if err := kube.Create(t.Context(), endpoints); err != nil {
			t.Fatalf("create %s/%s EndpointSlice: %v", namespace, name, err)
		}
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", args...)
	command.Dir = dir
	command.Env = append(command.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
