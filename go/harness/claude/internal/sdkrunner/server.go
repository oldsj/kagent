package sdkrunner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Subprotocol is the versioned client event protocol.
const Subprotocol = "kagent.sdk-runner.v1"

const (
	minTokenLength = 24
	// maxBundleBytes bounds the unpacked bootstrap config bundle.
	maxBundleBytes = 512 << 20
	maxClientFrame = 4 << 20
)

// Server serves the client port: PUT /bootstrap, GET /healthz and GET /ws.
// It is reached only through the Substrate router CONNECT.
type Server struct {
	session *Session
	ctx     context.Context

	mu    sync.Mutex
	token string
}

// NewServer loads any token a previous bootstrap stored.
func NewServer(ctx context.Context, session *Session) *Server {
	srv := &Server{session: session, ctx: ctx}
	if data, err := os.ReadFile(srv.tokenPath()); err == nil {
		srv.token = strings.TrimSpace(string(data))
	}
	return srv
}

func (srv *Server) tokenPath() string { return filepath.Join(srv.session.cfg.Dir, "token") }

func (srv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/bootstrap" && r.Method == http.MethodPut:
		srv.bootstrap(w, r)
	case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
		srv.mu.Lock()
		state := "needs-bootstrap"
		if srv.token != "" {
			state = "ready"
		}
		srv.mu.Unlock()
		_, _ = io.WriteString(w, state+"\n")
	case r.URL.Path == "/ws":
		srv.websocket(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (srv *Server) authorized(token string) bool {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(srv.token)) == 1
}

// bootstrap is trust on first use: the first caller sets the Session's client
// token and, optionally, its Claude user home. Later calls get 409.
func (srv *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("x-runner-token")
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.token != "" || len(token) < minTokenLength {
		http.Error(w, "conflict", http.StatusConflict)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxBundleBytes)
	installed, err := unpackHome(body, srv.session.HomeDir())
	if err != nil {
		srv.session.cfg.Logger.WarnContext(r.Context(), "sdk runner bootstrap rejected", "error", err)
		http.Error(w, "invalid bundle", http.StatusBadRequest)
		return
	}
	if err := writePrivate(srv.tokenPath(), token); err != nil {
		http.Error(w, "store token", http.StatusInternalServerError)
		return
	}
	srv.token = token
	srv.session.cfg.Logger.InfoContext(r.Context(), "sdk runner bootstrapped", "home", installed)
	if installed {
		// The next start applies the user profile and resumes the same session.
		_ = srv.session.Stop()
	}
	_, _ = io.WriteString(w, "ok\n")
}

func writePrivate(path, value string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// unpackHome extracts a tar.gz of regular files and directories into dir. An
// empty body installs nothing. Links, devices and paths leaving dir are
// rejected; the bundle is staged and renamed into place.
func unpackHome(body io.Reader, dir string) (bool, error) {
	var first [1]byte
	n, err := io.ReadFull(body, first[:])
	if n == 0 {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	gz, err := gzip.NewReader(io.MultiReader(strings.NewReader(string(first[:])), body))
	if err != nil {
		return false, err
	}
	stage := dir + ".staging"
	_ = os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return false, err
	}
	archive := tar.NewReader(gz)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = os.RemoveAll(stage)
			return false, err
		}
		name := filepath.Clean(header.Name)
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			_ = os.RemoveAll(stage)
			return false, fmt.Errorf("bundle path %q leaves the home directory", header.Name)
		}
		target := filepath.Join(stage, name)
		switch header.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o700)
		case tar.TypeReg:
			err = writeBundleFile(target, archive, os.FileMode(header.Mode).Perm()&0o700|0o600)
		default:
			err = fmt.Errorf("bundle entry %q has unsupported type %q", header.Name, header.Typeflag)
		}
		if err != nil {
			_ = os.RemoveAll(stage)
			return false, err
		}
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(stage, dir); err != nil {
		return false, err
	}
	return true, nil
}

func writeBundleFile(path string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, r)
	return errors.Join(copyErr, f.Close())
}

type clientMessage struct {
	K      string `json:"k"`
	ID     string `json:"id"`
	Text   string `json:"text"`
	UUID   string `json:"uuid"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Mode   string `json:"mode"`
}

func (srv *Server) websocket(w http.ResponseWriter, r *http.Request) {
	if !srv.authorized(r.URL.Query().Get("t")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var since int64
	if raw := r.URL.Query().Get("since"); raw != "" {
		since, _ = strconv.ParseInt(raw, 10, 64)
	}
	// The token authorizes the client; browsers reach this port through a
	// relay page on another origin.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(maxClientFrame)
	ctx, cancel := context.WithCancel(srv.ctx)
	defer cancel()
	c := newClient()
	hello, err := srv.session.attach(c, since)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "hello")
		return
	}
	defer srv.session.detach(c)
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		return
	}
	// Attaching is demand: start the bridge so the session is live.
	go func() {
		if err := srv.session.Start(ctx); err != nil {
			srv.sendError(c, "", err)
		}
	}()
	go func() {
		defer cancel()
		for {
			select {
			case data := <-c.send:
				writeCtx, done := context.WithTimeout(ctx, 30*time.Second)
				err := conn.Write(writeCtx, websocket.MessageText, data)
				done()
				if err != nil {
					return
				}
			case <-c.gone:
				_ = conn.Close(websocket.StatusPolicyViolation, "client fell behind")
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg clientMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if err := srv.command(ctx, msg); err != nil {
			srv.sendError(c, msg.ID, err)
		}
	}
}

func (srv *Server) command(ctx context.Context, msg clientMessage) error {
	switch msg.K {
	case "user", "interrupt", "model", "effort", "perm":
		// The A2A task is the record of what was asked of the agent and what
		// it was allowed to do. A client sends a message or a steer, cancels a
		// task, sets a turn's model and effort, and answers a permission
		// prompt through the agent's A2A endpoint, so none of them bypass
		// that record.
		return fmt.Errorf("%s is not accepted on the client port; use the agent's A2A endpoint", msg.K)
	case "mode":
		return srv.session.Control(ctx, "mode", msg.Mode, msg.ID)
	default:
		return nil // Unknown kinds are ignored within v1.
	}
}

func (srv *Server) sendError(c *client, ref string, err error) {
	data, _ := json.Marshal(map[string]any{"k": "err", "text": err.Error(), "ref": ref, "ts": time.Now().UnixMilli()})
	select {
	case c.send <- data:
	default:
	}
}
