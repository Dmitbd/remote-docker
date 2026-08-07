package tray

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/localapi"
)

const (
	defaultTimeout     = 5 * time.Second
	unavailableMessage = "The background agent is unavailable. Retry or run diagnostics."
)

// Client is deliberately limited to the existing owner-only local control API.
// The tray does not read configuration or credentials and cannot invoke Docker,
// WSL, or operating-system processes directly.
type Client interface {
	Call(context.Context, localapi.Method, any, any) error
}

// Controller owns temporary presentation state only. All agent work is sent to
// the background agent over Client.
type Controller struct {
	Client  Client
	Timeout time.Duration
	Present func(context.Context, Model)
	QuitUI  func(context.Context)

	mu      sync.RWMutex
	model   Model
	pairing *Pairing
}

func NewController(client Client) *Controller {
	return &Controller{
		Client: client,
		model:  MenuForStatus(localapi.StatusResult{State: "NeedsAction", Message: "Open status to connect to the background agent."}),
	}
}

func (c *Controller) Current() Model {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneModel(c.model)
}

func (c *Controller) OpenStatus(ctx context.Context) (Model, error) {
	var status localapi.StatusResult
	if err := c.call(ctx, localapi.MethodStatus, nil, &status); err != nil {
		return c.setUnavailable()
	}
	return c.setStatus(status), nil
}

// Pair starts a pairing session. The chosen device name is presentation input;
// the API receives the same selector and the agent remains the sole owner of
// discovery and pairing credentials.
func (c *Controller) Pair(ctx context.Context, deviceName string) (Model, error) {
	deviceName = strings.TrimSpace(deviceName)
	var result localapi.PairStartResult
	if err := c.call(ctx, localapi.MethodPairStart, localapi.PairStartParams{Device: deviceName}, &result); err != nil {
		return c.setUnavailable()
	}
	if result.SessionID == "" || !sixDigits(result.Code) {
		return c.setMessage("Pairing could not be started.")
	}
	if deviceName == "" {
		deviceName = "Automatically discovered device"
	}
	pairing := &Pairing{DeviceName: deviceName, Code: result.Code, sessionID: result.SessionID}
	c.mu.Lock()
	c.pairing = pairing
	c.model.Pairing = clonePairing(pairing)
	c.model.Items = append(c.model.Items, Item{Action: ActionConfirmPair, Label: "Confirm pairing", Enabled: true})
	model := cloneModel(c.model)
	c.mu.Unlock()
	c.present(model)
	return model, nil
}

// ConfirmPair is intentionally separate from Pair: no API confirmation can be
// sent until a person invokes this action.
func (c *Controller) ConfirmPair(ctx context.Context) (Model, error) {
	c.mu.RLock()
	pairing := clonePairing(c.pairing)
	c.mu.RUnlock()
	if pairing == nil {
		return c.setMessage("Start pairing before confirming it.")
	}
	var result localapi.PairConfirmResult
	if err := c.call(ctx, localapi.MethodPairConfirm, localapi.PairConfirmParams{
		SessionID: pairing.sessionID, Code: pairing.Code,
	}, &result); err != nil {
		return c.setUnavailable()
	}
	c.mu.Lock()
	c.pairing = nil
	c.mu.Unlock()
	return c.OpenStatus(ctx)
}

func (c *Controller) AddWorkspace(ctx context.Context, path string) (Model, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return c.setMessage("Choose a workspace directory before adding it.")
	}
	var result localapi.Workspace
	if err := c.call(ctx, localapi.MethodWorkspaceAdd, localapi.WorkspaceAddParams{Path: path}, &result); err != nil {
		return c.setUnavailable()
	}
	return c.setStatusMessage("Workspace added."), nil
}

func (c *Controller) Retry(ctx context.Context) (Model, error) {
	var result localapi.RecoverResult
	if err := c.call(ctx, localapi.MethodRecover, nil, &result); err != nil {
		return c.setUnavailable()
	}
	return c.setStatus(localapi.StatusResult{State: result.State, Message: result.Message}), nil
}

func (c *Controller) RunDiagnostics(ctx context.Context) (Model, error) {
	var result localapi.DoctorResult
	if err := c.call(ctx, localapi.MethodDoctor, nil, &result); err != nil {
		return c.setUnavailable()
	}
	return c.setStatusMessage("Diagnostics completed."), nil
}

func (c *Controller) Unpair(ctx context.Context, deviceID string) (Model, error) {
	var result map[string]bool
	if err := c.call(ctx, localapi.MethodUnpair, localapi.UnpairParams{DeviceID: strings.TrimSpace(deviceID)}, &result); err != nil {
		return c.setUnavailable()
	}
	c.mu.Lock()
	c.pairing = nil
	c.mu.Unlock()
	return c.setStatus(localapi.StatusResult{State: "Unpaired", Message: "Device unpaired."}), nil
}

func (c *Controller) Quit(ctx context.Context) {
	if c == nil || c.QuitUI == nil {
		return
	}
	c.invokePresentation(c.QuitUI, ctx)
}

func (c *Controller) call(ctx context.Context, method localapi.Method, params, result any) error {
	if c == nil || c.Client == nil {
		return errors.New("local control client is unavailable")
	}
	callCtx, cancel := c.boundedContext(ctx)
	defer cancel()
	return c.Client.Call(callCtx, method, params, result)
}

func (c *Controller) boundedContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return context.WithTimeout(parent, timeout)
}

func (c *Controller) setStatus(status localapi.StatusResult) Model {
	model := MenuForStatus(status)
	c.mu.Lock()
	if c.pairing != nil {
		model.Pairing = clonePairing(c.pairing)
		model.Items = append(model.Items, Item{Action: ActionConfirmPair, Label: "Confirm pairing", Enabled: true})
	}
	c.model = model
	current := cloneModel(c.model)
	c.mu.Unlock()
	c.present(current)
	return current
}

func (c *Controller) setMessage(message string) (Model, error) {
	model := c.setStatusMessage(message)
	return model, errors.New(message)
}

func (c *Controller) setStatusMessage(message string) Model {
	c.mu.Lock()
	c.model.Message = message
	model := cloneModel(c.model)
	c.mu.Unlock()
	c.present(model)
	return model
}

func (c *Controller) setUnavailable() (Model, error) {
	return c.setMessage(unavailableMessage)
}

func (c *Controller) present(model Model) {
	if c == nil || c.Present == nil {
		return
	}
	c.invokePresentation(func(ctx context.Context) { c.Present(ctx, model) }, context.Background())
}

func (c *Controller) invokePresentation(callback func(context.Context), parent context.Context) {
	ctx, cancel := c.boundedContext(parent)
	go func() {
		defer cancel()
		callback(ctx)
	}()
}

func sixDigits(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, character := range code {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func clonePairing(pairing *Pairing) *Pairing {
	if pairing == nil {
		return nil
	}
	copy := *pairing
	return &copy
}

func cloneModel(model Model) Model {
	model.Items = append([]Item(nil), model.Items...)
	model.Pairing = clonePairing(model.Pairing)
	return model
}
