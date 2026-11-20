package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

const osUser = "db-branch-rt-clickhouse"

type module struct {
	pb.UnimplementedRuntimeServer
	http *http.Client
}

func newModule() *module { return &module{http: &http.Client{Timeout: 5 * time.Minute}} }

func internal(err error) error { return status.Error(codes.Internal, err.Error()) }
func ok() (*pb.Empty, error)   { return &pb.Empty{}, nil }

func (m *module) Describe(context.Context, *pb.DescribeRequest) (*pb.EngineSpec, error) {
	return &pb.EngineSpec{
		Engine: "clickhouse", DisplayName: "ClickHouse", RunAsUser: osUser, VersionPattern: `^[0-9]+\.[0-9]+(\.[0-9]+)*$`,
		ZfsProps:     map[string]string{"recordsize": "128K", "compression": "lz4"},
		Protocols:    []*pb.ProtocolPort{{Name: "http", Primary: true}},
		Capabilities: &pb.Capabilities{Query: true, Checkpoints: true},
	}, nil
}

// Catalog: the prototype lists no downloads; the runtime is put in the
// runtime directory by hand (a pinned archive would be listed here).
func (m *module) Catalog(context.Context, *pb.CatalogRequest) (*pb.CatalogResponse, error) {
	return &pb.CatalogResponse{}, nil
}

func (m *module) PrepareRuntime(ctx context.Context, r *pb.PrepareRuntimeRequest) (*pb.Empty, error) {
	bin := filepath.Join(r.StagingDir, "clickhouse")
	if out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput(); err != nil {
		return nil, internal(fmt.Errorf("%s does not run: %v: %.200s", bin, err, out))
	}
	return ok()
}

func (m *module) Initialize(_ context.Context, r *pb.InstanceRequest) (*pb.Empty, error) {
	for _, d := range []string{"tmp", "user_files", "format_schemas", "access"} {
		if err := os.MkdirAll(filepath.Join(r.Instance.DataDir, d), 0o750); err != nil {
			return nil, internal(err)
		}
	}
	return ok()
}

// PrepareClone removes what ties a copy to its source: the status file a
// running server keeps (it holds the PID).
func (m *module) PrepareClone(_ context.Context, r *pb.InstanceRequest) (*pb.Empty, error) {
	_ = os.Remove(filepath.Join(r.Instance.DataDir, "status"))
	return ok()
}

func (m *module) PrepareStart(_ context.Context, r *pb.InstanceRequest) (*pb.LaunchPlan, error) {
	i := r.Instance
	if i.Ports["http"] == 0 {
		return nil, status.Error(codes.InvalidArgument, "no http port was allocated")
	}
	if _, err := m.Initialize(context.Background(), r); err != nil {
		return nil, err
	}
	logins, err := readLogins(i.StateDir)
	if err != nil {
		return nil, internal(err)
	}
	if err := writeAtomic(filepath.Join(i.StateDir, "users.xml"), usersXML(logins)); err != nil {
		return nil, internal(err)
	}
	cfg := filepath.Join(i.StateDir, "config.xml")
	if err := writeAtomic(cfg, serverConfig(i, i.Maintenance)); err != nil {
		return nil, internal(err)
	}
	return &pb.LaunchPlan{
		Command: filepath.Join(i.RuntimeDir, "clickhouse"), Args: []string{"server", "--config-file=" + cfg},
		ServiceType: "simple", KillSignal: "SIGTERM", TimeoutStopSec: 120,
		// `clickhouse server` is a watchdog and a server: a SIGTERM to both makes the
		// server take the second one for "stop now" and skip its clean shutdown.
		KillMode: "mixed",
	}, nil
}

func baseURL(i *pb.InstanceSpec) string {
	host := i.Bind
	if host == "" || i.Maintenance {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(int(i.Ports["http"])))
}

func (m *module) Health(ctx context.Context, r *pb.InstanceRequest) (*pb.HealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(r.Instance)+"/ping", nil)
	resp, err := m.http.Do(req)
	if err != nil {
		return &pb.HealthResponse{State: pb.HealthState_HEALTH_STATE_STOPPED}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return &pb.HealthResponse{State: pb.HealthState_HEALTH_STATE_READY}, nil
	}
	return &pb.HealthResponse{State: pb.HealthState_HEALTH_STATE_STARTING}, nil
}

// Quiesce: a MergeTree table is durable once its insert returned, and the
// server flushes the rest when it stops on SIGTERM.
func (m *module) Quiesce(context.Context, *pb.QuiesceRequest) (*pb.Empty, error) { return ok() }

// VerifyCleanShutdown: a server that shut down cleanly removes its status file.
func (m *module) VerifyCleanShutdown(_ context.Context, r *pb.InstanceRequest) (*pb.Empty, error) {
	if b, err := os.ReadFile(filepath.Join(r.Instance.DataDir, "status")); err == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "the status file is still there: the server did not shut down cleanly (%q)", strings.TrimSpace(string(b)))
	}
	return ok()
}

func (m *module) RemoveInstance(_ context.Context, r *pb.InstanceRequest) (*pb.Empty, error) {
	for _, f := range []string{"config.xml", "users.xml", "logins.json", "clickhouse-server.log", "clickhouse-server.err.log"} {
		_ = os.Remove(filepath.Join(r.Instance.StateDir, f))
	}
	return ok()
}

// CreateUser makes or updates the login; users.xml is rewritten and the
// running server re-reads it.
func (m *module) CreateUser(_ context.Context, r *pb.CreateUserRequest) (*pb.Empty, error) {
	logins, err := readLogins(r.Instance.StateDir)
	if err != nil {
		return nil, internal(err)
	}
	logins[r.User] = hashPassword(r.Password)
	if err := writeLogins(r.Instance.StateDir, logins); err != nil {
		return nil, internal(err)
	}
	if err := writeAtomic(filepath.Join(r.Instance.StateDir, "users.xml"), usersXML(logins)); err != nil {
		return nil, internal(err)
	}
	return ok()
}

// ExecAs runs init SQL as the login, one statement at a time.
func (m *module) ExecAs(ctx context.Context, r *pb.ExecAsRequest) (*pb.Empty, error) {
	for _, stmt := range splitStatements(r.Sql) {
		if _, err := m.run(ctx, r.Instance, "", r.User, r.Password, stmt, 0); err != nil {
			return nil, err
		}
	}
	return ok()
}

func (m *module) AdminAccess(_ context.Context, r *pb.InstanceRequest) (*pb.AdminAccessInfo, error) {
	i := r.Instance
	return &pb.AdminAccessInfo{
		Engine: "clickhouse", Host: "127.0.0.1", Port: i.Ports["http"], User: "admin",
		BinDir: i.RuntimeDir, LibDir: i.RuntimeDir,
		Endpoints: map[string]*pb.Endpoint{"http": {Host: "127.0.0.1", Port: i.Ports["http"]}},
	}, nil
}

func (m *module) FormatDSN(_ context.Context, r *pb.FormatDSNRequest) (*pb.FormatDSNResponse, error) {
	u := url.URL{Scheme: "clickhouse", User: url.UserPassword(r.User, r.Password),
		Host: net.JoinHostPort(r.Host, strconv.Itoa(int(r.Instance.Ports["http"]))), Path: "/"}
	return &pb.FormatDSNResponse{Dsn: u.String()}, nil
}

func (m *module) Query(ctx context.Context, r *pb.QueryRequest) (*pb.QueryResponse, error) {
	start := time.Now()
	body, err := m.run(ctx, r.Instance, r.Host, r.User, r.Password, r.Sql, int(r.MaxRows))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 { // a statement without a result (DDL, INSERT)
		return &pb.QueryResponse{HasAffected: true, Millis: time.Since(start).Milliseconds()}, nil
	}
	res, err := parseResult(body, int(r.MaxRows))
	if err != nil {
		return nil, internal(err)
	}
	res.Millis = time.Since(start).Milliseconds()
	return res, nil
}

// run sends one statement to the HTTP interface as user. Results come as
// JSONCompact; ClickHouse itself stops reading after maxRows (+1, to tell a
// cut from an exact fit).
func (m *module) run(ctx context.Context, i *pb.InstanceSpec, host, user, password, sql string, maxRows int) ([]byte, error) {
	base := baseURL(i)
	if host != "" {
		base = "http://" + net.JoinHostPort(host, strconv.Itoa(int(i.Ports["http"])))
	}
	q := url.Values{"default_format": {"JSONCompact"}}
	if maxRows > 0 {
		q.Set("max_result_rows", strconv.Itoa(maxRows+1))
		q.Set("result_overflow_mode", "break")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/?"+q.Encode(), strings.NewReader(sql))
	if err != nil {
		return nil, internal(err)
	}
	req.SetBasicAuth(user, password)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, status.Error(codes.Internal, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (m *module) TailLog(_ context.Context, r *pb.TailLogRequest) (*pb.TailLogResponse, error) {
	b, err := os.ReadFile(filepath.Join(r.Instance.StateDir, "clickhouse-server.log"))
	if err != nil {
		return nil, internal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if n := int(r.Lines); n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return &pb.TailLogResponse{Text: strings.Join(lines, "\n") + "\n"}, nil
}

func readLogins(stateDir string) (map[string]string, error) {
	logins := map[string]string{}
	b, err := os.ReadFile(filepath.Join(stateDir, "logins.json"))
	if os.IsNotExist(err) {
		return logins, nil
	}
	if err != nil {
		return nil, err
	}
	return logins, json.Unmarshal(b, &logins)
}

func writeLogins(stateDir string, logins map[string]string) error {
	b, _ := json.Marshal(logins)
	return writeAtomic(filepath.Join(stateDir, "logins.json"), string(b))
}

func writeAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
