package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

const (
	osUser = "db-branch-rt-clickhouse"

	// defaultFreezeTimeout caps how long PreFreeze waits for running merges to
	// drain (matching the core's defaultFreezeTimeout of 5s in livefreeze.go).
	defaultFreezeTimeout      = 5 * time.Second
	defaultFreezePollInterval = 50 * time.Millisecond
	unfreezeRollbackTimeout   = 5 * time.Second
)

type module struct {
	pb.UnimplementedRuntimeServer
	http               *http.Client
	freezeTimeout      time.Duration
	freezePollInterval time.Duration
}

// newModule's HTTP client does not keep connections: ClickHouse waits for the
// connections left open when it is told to stop (5 seconds, then it ends without
// its clean shutdown), and the module would be the one holding them.
func newModule() *module {
	return &module{
		http:               &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{DisableKeepAlives: true}},
		freezeTimeout:      defaultFreezeTimeout,
		freezePollInterval: defaultFreezePollInterval,
	}
}

func internal(err error) error { return status.Error(codes.Internal, err.Error()) }
func ok() (*pb.Empty, error)   { return &pb.Empty{}, nil }

func (m *module) Describe(context.Context, *pb.DescribeRequest) (*pb.EngineSpec, error) {
	return &pb.EngineSpec{
		Engine: "clickhouse", DisplayName: "ClickHouse", RunAsUser: osUser, VersionPattern: `^[0-9]+\.[0-9]+(\.[0-9]+)*$`,
		ZfsProps:     map[string]string{"recordsize": "128K", "compression": "lz4"},
		Protocols:    []*pb.ProtocolPort{{Name: "http", Primary: true}},
		Capabilities: &pb.Capabilities{Query: true, Checkpoints: true, LiveFreeze: true, Upgrade: true},
	}, nil
}

// releases are the builds db-branch may download: ClickHouse's own LTS archives
// ("common-static" holds the clickhouse binary), pinned by SHA-256. To add a
// release, download it, check ClickHouse's published .sha512, and add the SHA-256.
var releases = []*pb.CatalogEntry{
	{Version: "26.3.19.3", Platform: "amd64", Size: 223623706,
		Url:    "https://packages.clickhouse.com/tgz/lts/clickhouse-common-static-26.3.19.3-amd64.tgz",
		Sha256: "8c9886db49555efb6bef2ee08656af0d48bd07c83a08a729abb9d60e26bfff3d"},
	{Version: "26.3.19.3", Platform: "arm64", Size: 203799350,
		Url:    "https://packages.clickhouse.com/tgz/lts/clickhouse-common-static-26.3.19.3-arm64.tgz",
		Sha256: "f7df37b5c600099bc6e772c9bae2b91a530c6bbc07a2bd5a5021443c742c4c92"},
}

func (m *module) Catalog(context.Context, *pb.CatalogRequest) (*pb.CatalogResponse, error) {
	return &pb.CatalogResponse{Entries: releases}, nil
}

// binary is the clickhouse executable of an unpacked runtime: usr/bin/clickhouse
// in ClickHouse's archive, or a clickhouse placed by hand in the directory.
func binary(runtimeDir string) string {
	packaged := filepath.Join(runtimeDir, "usr", "bin", "clickhouse")
	if _, err := os.Stat(packaged); err == nil {
		return packaged
	}
	return filepath.Join(runtimeDir, "clickhouse")
}

func (m *module) PrepareRuntime(ctx context.Context, r *pb.PrepareRuntimeRequest) (*pb.Empty, error) {
	bin := binary(r.StagingDir)
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
		Command: binary(i.RuntimeDir), Args: []string{"server", "--config-file=" + cfg},
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

func adminHost(i *pb.InstanceSpec) string {
	host := i.GetBind()
	if host == "" || host == "0.0.0.0" || host == "::" || i.GetMaintenance() {
		return "127.0.0.1"
	}
	return host
}

func (m *module) runAdmin(ctx context.Context, i *pb.InstanceSpec, sql string) ([]byte, error) {
	return m.run(ctx, i, adminHost(i), "admin", "", sql, 0)
}

func (m *module) resumeBackground(ctx context.Context, i *pb.InstanceSpec) error {
	_, errMoves := m.runAdmin(ctx, i, "SYSTEM START MOVES")
	_, errMerges := m.runAdmin(ctx, i, "SYSTEM START MERGES")
	switch {
	case errMoves != nil && errMerges != nil:
		return internal(errors.Join(errMoves, errMerges))
	case errMoves != nil:
		return errMoves
	default:
		return errMerges
	}
}

// PreFreeze flushes pending async inserts, stops background merges and moves,
// and waits (up to freezeTimeout) for in-flight merges to finish so ZFS takes
// a cleaner snapshot. On any failure or timeout it resumes moves and merges
// before returning the error.
func (m *module) PreFreeze(ctx context.Context, r *pb.InstanceRequest) (*pb.FreezeToken, error) {
	i := r.GetInstance()
	if i == nil || i.Ports["http"] == 0 {
		return nil, status.Error(codes.InvalidArgument, "no http port was allocated")
	}
	waitCtx, cancel := context.WithTimeout(ctx, m.freezeTimeout)
	defer cancel()

	if _, err := m.runAdmin(waitCtx, i, "SYSTEM FLUSH ASYNC INSERT QUEUE"); err != nil {
		return nil, err
	}
	rollback := func() error {
		rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), unfreezeRollbackTimeout)
		defer rbCancel()
		return m.resumeBackground(rbCtx, i)
	}
	if _, err := m.runAdmin(waitCtx, i, "SYSTEM STOP MERGES"); err != nil {
		_ = rollback()
		return nil, err
	}
	if _, err := m.runAdmin(waitCtx, i, "SYSTEM STOP MOVES"); err != nil {
		_ = rollback()
		return nil, err
	}

	var remaining int64
	for {
		body, err := m.runAdmin(waitCtx, i, "SELECT count() FROM system.merges")
		if err != nil {
			rbErr := rollback()
			if waitCtx.Err() != nil {
				return nil, freezeTimeoutErr(remaining, rbErr)
			}
			return nil, err
		}
		counts, err := parseCounts(body, 1)
		if err != nil {
			_ = rollback()
			return nil, internal(err)
		}
		remaining = counts[0]
		if remaining == 0 {
			return &pb.FreezeToken{
				Data: encodeFreezeToken(freezeState{
					Host:          adminHost(i),
					Port:          i.Ports["http"],
					StoppedMerges: true,
					StoppedMoves:  true,
				}),
			}, nil
		}
		timer := time.NewTimer(m.freezePollInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			rbErr := rollback()
			return nil, freezeTimeoutErr(remaining, rbErr)
		case <-timer.C:
		}
	}
}

func freezeTimeoutErr(remaining int64, rbErr error) error {
	if rbErr != nil {
		return status.Errorf(codes.DeadlineExceeded, "timed out waiting for %d running merge(s) to finish (rollback failed: %v)", remaining, rbErr)
	}
	return status.Errorf(codes.DeadlineExceeded, "timed out waiting for %d running merge(s) to finish", remaining)
}

// PostFreeze resumes background moves and merges. It is idempotent and works
// even with an empty token (crash recovery after an agent restart).
func (m *module) PostFreeze(ctx context.Context, r *pb.PostFreezeRequest) (*pb.Empty, error) {
	i := r.GetInstance()
	if i == nil || i.Ports["http"] == 0 {
		if st, ok := decodeFreezeToken(r.GetToken().GetData()); ok && st.Port != 0 {
			i = &pb.InstanceSpec{Bind: st.Host, Ports: map[string]int32{"http": st.Port}}
		} else {
			return nil, status.Error(codes.InvalidArgument, "no http port was allocated")
		}
	}
	if err := m.resumeBackground(ctx, i); err != nil {
		return nil, err
	}
	return ok()
}

// CheckUpgradePath allows moving to a strictly newer ClickHouse version and
// refuses the same version or any downgrade. Skipping LTS or minor releases is
// allowed here (the RPC carries no warnings) and reported as a warning in
// CheckUpgrade's UpgradePlan.
func (m *module) CheckUpgradePath(_ context.Context, r *pb.CheckUpgradePathRequest) (*pb.Empty, error) {
	from, to := r.GetFromVersion(), r.GetToVersion()
	if _, err := parseCHVersion(from); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "from version: %v", err)
	}
	if _, err := parseCHVersion(to); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "to version: %v", err)
	}
	if err := checkUpgradePath(from, to); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return ok()
}

func hasInstalledBinary(currentRuntimeDir, toVersion string) bool {
	if currentRuntimeDir == "" {
		return false
	}
	targetDir := filepath.Join(filepath.Dir(filepath.Clean(currentRuntimeDir)), toVersion)
	fi, err := os.Stat(binary(targetDir))
	return err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0
}

const upgradeInspectSQL = "SELECT (SELECT count() FROM system.tables WHERE database NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema')), " +
	"(SELECT count() FROM system.parts WHERE active AND database NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema'))"

// CheckUpgrade verifies without side effects that the target runtime is
// installed, DataDir is readable, and the running ClickHouse server answers a
// lightweight metadata query.
func (m *module) CheckUpgrade(ctx context.Context, r *pb.CheckUpgradeRequest) (*pb.UpgradePlan, error) {
	i := r.GetInstance()
	to := r.GetToVersion()
	if i == nil || to == "" {
		return nil, status.Error(codes.InvalidArgument, "instance and to_version are required")
	}
	if i.Ports["http"] == 0 {
		return nil, status.Error(codes.InvalidArgument, "no http port was allocated")
	}
	if _, err := parseCHVersion(to); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "to version: %v", err)
	}
	if i.DataDir == "" {
		return nil, status.Error(codes.FailedPrecondition, "data directory is empty")
	}
	if _, err := os.ReadDir(i.DataDir); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "data directory %s is not readable: %v", i.DataDir, err)
	}
	body, err := m.runAdmin(ctx, i, upgradeInspectSQL)
	if err != nil {
		return nil, err
	}
	counts, err := parseCounts(body, 2)
	if err != nil {
		return nil, internal(err)
	}

	plan := &pb.UpgradePlan{}
	if i.Version != "" {
		if err := checkUpgradePath(i.Version, to); err != nil {
			plan.Findings = append(plan.Findings, &pb.Finding{
				Level: "error", Code: "upgrade_path", Message: err.Error(),
			})
		} else {
			plan.Findings = append(plan.Findings, upgradePathWarnings(i.Version, to)...)
		}
	}
	if !hasInstalledBinary(i.RuntimeDir, to) {
		plan.Findings = append(plan.Findings, &pb.Finding{
			Level:   "error",
			Code:    "runtime_not_installed",
			Message: fmt.Sprintf("ClickHouse %s runtime binary is not installed", to),
		})
	}
	fromLabel := i.Version
	if fromLabel == "" {
		fromLabel = "current"
	}
	plan.Findings = append(plan.Findings, &pb.Finding{
		Level:   "info",
		Code:    "upgrade_path",
		Message: fmt.Sprintf("ClickHouse %s to %s: %d user table(s), %d active part(s)", fromLabel, to, counts[0], counts[1]),
	})
	return plan, nil
}

// StartUpgraded prepares configuration and LaunchPlan for the new ClickHouse
// runtime using the same protocol and user rules as PrepareStart; the core
// stops the old server and starts the new one.
func (m *module) StartUpgraded(ctx context.Context, r *pb.StartUpgradedRequest) (*pb.StartUpgradedResponse, error) {
	i := r.GetInstance()
	if i == nil {
		return nil, status.Error(codes.InvalidArgument, "instance is required")
	}
	if i.Ports["http"] == 0 {
		return nil, status.Error(codes.InvalidArgument, "no http port was allocated")
	}
	if login := r.GetLogin(); login != nil && login.GetUser() != "" && login.GetPassword() != "" {
		if _, err := m.CreateUser(ctx, &pb.CreateUserRequest{Instance: i, User: login.User, Password: login.Password}); err != nil {
			return nil, err
		}
	}
	plan, err := m.PrepareStart(ctx, &pb.InstanceRequest{Instance: i})
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("starting upgraded ClickHouse with %s", plan.Command)
	if i.Version != "" {
		msg = fmt.Sprintf("starting ClickHouse %s with %s", i.Version, plan.Command)
	}
	return &pb.StartUpgradedResponse{
		Plan: plan,
		Findings: []*pb.Finding{{
			Level:   "info",
			Code:    "log_upgrade",
			Message: msg,
		}},
	}, nil
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
