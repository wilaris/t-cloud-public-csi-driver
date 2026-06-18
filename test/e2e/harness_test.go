//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/report"
)

const (
	driverStartTimeout = 90 * time.Second
	probeRetryInterval = time.Second
)

// harness holds everything one run shares. It is package state because the
// process-wide setup belongs to TestMain and every scenario reads the same
// approved environment.
type harness struct {
	env      *environment
	identity *instanceIdentity

	provider *golangsdk.ProviderClient
	evs      *evs.Client
	v3       *golangsdk.ServiceClient
	v2       *golangsdk.ServiceClient
	ecs      *golangsdk.ServiceClient

	runID  string
	prefix string
	runDir string

	// runCtx is the run's wall-clock bound.
	// Teardown uses a separate budget so an expired run can still delete what it created.
	runCtx context.Context

	ledger      *ledger
	evidence    *report.Evidence
	progress    *report.Progress
	preexisting map[string]bool
	selected    []catalogue.Check
	selectedIDs map[catalogue.CheckID]bool

	header      report.ReportHeader
	progressOut *report.MaskingWriter
	outputs     report.OutputDestinations

	// transcript collects everything the run printed, so the containment
	// check covers the readable report as well as the machine record.
	transcript lockedBuffer

	controller *driverProcess
	node       *driverProcess
}

// runState is set once by TestMain before any scenario runs.
var runState *harness

// budgetExhausted reports whether the run has passed its own wall-clock
// bound. A check that starts after that point would only fail against a
// cancelled context, so it is reported as unreached.
func (h *harness) budgetExhausted() bool {
	return h.runCtx != nil && h.runCtx.Err() != nil
}

// context returns the bounded context every scenario runs under.
func (h *harness) context() context.Context {
	return h.runCtx
}

// lockedBuffer collects a driver process's output for the containment
// check that no credential reached a diagnostic.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// driverProcess is one role of the real driver binary, serving its real socket.
type driverProcess struct {
	role     config.Role
	endpoint string
	cmd      *exec.Cmd
	output   *lockedBuffer
	conn     *grpc.ClientConn
}

// startDriver runs one role as an ordinary subprocess. Credentials reach
// the controller through its process environment only; the node role
// receives none at all.
func (h *harness) startDriver(ctx context.Context, role config.Role) (*driverProcess, error) {
	socket := filepath.Join(h.runDir, string(role)+".sock")
	endpoint := "unix://" + socket

	environ := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.runDir,
		"LOG_LEVEL=debug",
	}
	if role == config.RoleController {
		environ = append(environ, h.env.cloudEnviron()...)
	}

	args := []string{"--role=" + string(role), "--endpoint=" + endpoint}
	if err := h.env.assertContained("driver command line", args...); err != nil {
		return nil, err
	}

	output := &lockedBuffer{}
	//nolint:gosec // the binary path is validated operator input and the arguments are built here
	cmd := exec.CommandContext(ctx, h.env.driverBinary, args...)
	cmd.Env = environ
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s role: %w", role, err)
	}

	process := &driverProcess{role: role, endpoint: endpoint, cmd: cmd, output: output}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = process.stop(ctx)
		return nil, fmt.Errorf("dial %s role: %w", role, err)
	}
	process.conn = conn

	if err := process.awaitReady(ctx); err != nil {
		_ = process.stop(ctx)
		return nil, err
	}
	return process, nil
}

// awaitReady blocks until the role answers a readiness probe over its own socket.
func (p *driverProcess) awaitReady(ctx context.Context) error {
	client := csi.NewIdentityClient(p.conn)

	lastAnswer, err := poll(ctx, probeRetryInterval, driverStartTimeout,
		func(ctx context.Context) (error, bool, error) {
			probeCtx, cancel := context.WithTimeout(ctx, probeRetryInterval*2)
			resp, probeErr := client.Probe(probeCtx, &csi.ProbeRequest{})
			cancel()
			if probeErr == nil {
				if resp.GetReady() == nil || resp.GetReady().GetValue() {
					return nil, true, nil
				}
				probeErr = errors.New("the probe answered not ready")
			}
			return probeErr, false, nil
		})
	if errors.Is(err, errPollTimeout) {
		return fmt.Errorf(
			"%s role did not become ready: %w (output: %s)",
			p.role,
			lastAnswer,
			p.output.String(),
		)
	}
	return err
}

// stop signals the role and waits for it to exit, escalating if it does not.
func (p *driverProcess) stop(ctx context.Context) error {
	var closeErr error
	if p.conn != nil {
		closeErr = p.conn.Close()
	}
	if err := stopProcess(ctx, p.cmd); err != nil {
		return errors.Join(closeErr, fmt.Errorf("stop %s role: %w", p.role, err))
	}
	return closeErr
}

func (p *driverProcess) identityClient() csi.IdentityClient {
	return csi.NewIdentityClient(p.conn)
}

func (p *driverProcess) controllerClient() csi.ControllerClient {
	return csi.NewControllerClient(p.conn)
}

func (p *driverProcess) nodeClient() csi.NodeClient {
	return csi.NewNodeClient(p.conn)
}

// diagnostics returns everything a role wrote, so the containment check can read it.
func (h *harness) diagnostics() []string {
	var collected []string
	for _, process := range []*driverProcess{h.controller, h.node} {
		if process != nil {
			collected = append(collected, process.output.String())
		}
	}
	return collected
}
