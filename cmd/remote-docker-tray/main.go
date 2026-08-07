package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sync"

	"fyne.io/systray"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/tray"
)

var icons = map[tray.Icon][]byte{
	tray.IconUnpaired:    statusIcon(color.RGBA{R: 128, G: 128, B: 128, A: 255}),
	tray.IconConnecting:  statusIcon(color.RGBA{R: 54, G: 123, B: 245, A: 255}),
	tray.IconStarting:    statusIcon(color.RGBA{R: 230, G: 152, B: 0, A: 255}),
	tray.IconSyncing:     statusIcon(color.RGBA{R: 119, G: 73, B: 204, A: 255}),
	tray.IconReady:       statusIcon(color.RGBA{R: 37, G: 145, B: 70, A: 255}),
	tray.IconDegraded:    statusIcon(color.RGBA{R: 206, G: 99, B: 0, A: 255}),
	tray.IconNeedsAction: statusIcon(color.RGBA{R: 190, G: 43, B: 43, A: 255}),
}

func main() {
	device := flag.String("device", "", "paired device name to select when starting pairing")
	workspace := flag.String("workspace", "", "workspace directory to add from the tray menu")
	flag.Parse()

	presentation := newPresentation(*device, *workspace)
	systray.Run(presentation.ready, nil)
}

type presentation struct {
	controller *tray.Controller
	device     string
	workspace  string

	mu       sync.Mutex
	items    map[tray.Action]*systray.MenuItem
	pairing  *systray.MenuItem
	pairCode *systray.MenuItem
	confirm  *systray.MenuItem
}

func newPresentation(device, workspace string) *presentation {
	controller := tray.NewController(localapi.Client{})
	presentation := &presentation{
		controller: controller,
		device:     device,
		workspace:  workspace,
		items:      make(map[tray.Action]*systray.MenuItem),
	}
	controller.Present = presentation.apply
	controller.QuitUI = func(context.Context) { systray.Quit() }
	return presentation
}

func (p *presentation) ready() {
	systray.SetIcon(iconFor(tray.IconNeedsAction))
	systray.SetTitle("Remote Docker")
	systray.SetTooltip("Remote Docker background agent")

	for _, item := range p.controller.Current().Items {
		menuItem := systray.AddMenuItem(item.Label, "")
		p.items[item.Action] = menuItem
		p.listen(item.Action, menuItem)
	}
	systray.AddSeparator()
	p.pairing = systray.AddMenuItem("No pairing is pending", "")
	p.pairing.Disable()
	p.pairCode = systray.AddMenuItem("", "")
	p.pairCode.Disable()
	p.confirm = systray.AddMenuItem("Confirm pairing", "")
	p.confirm.Disable()
	p.listen(tray.ActionConfirmPair, p.confirm)

	p.apply(context.Background(), p.controller.Current())
	go func() { _, _ = p.controller.OpenStatus(context.Background()) }()
}

func (p *presentation) listen(action tray.Action, item *systray.MenuItem) {
	go func() {
		for range item.ClickedCh {
			go p.invoke(action)
		}
	}()
}

func (p *presentation) invoke(action tray.Action) {
	ctx := context.Background()
	switch action {
	case tray.ActionPair:
		_, _ = p.controller.Pair(ctx, p.device)
	case tray.ActionOpenStatus:
		_, _ = p.controller.OpenStatus(ctx)
	case tray.ActionAddWorkspace:
		_, _ = p.controller.AddWorkspace(ctx, p.workspace)
	case tray.ActionRetry:
		_, _ = p.controller.Retry(ctx)
	case tray.ActionRunDiagnostics:
		_, _ = p.controller.RunDiagnostics(ctx)
	case tray.ActionUnpair:
		_, _ = p.controller.Unpair(ctx, "")
	case tray.ActionConfirmPair:
		_, _ = p.controller.ConfirmPair(ctx)
	case tray.ActionQuit:
		p.controller.Quit(ctx)
	}
}

func (p *presentation) apply(_ context.Context, model tray.Model) {
	p.mu.Lock()
	defer p.mu.Unlock()

	systray.SetIcon(iconFor(model.Icon))
	systray.SetTitle(fmt.Sprintf("Remote Docker — %s", model.Label))
	tooltip := model.Label
	if model.Message != "" {
		tooltip += ": " + model.Message
	}
	systray.SetTooltip(tooltip)

	for _, item := range model.Items {
		menuItem := p.items[item.Action]
		if menuItem == nil {
			continue
		}
		menuItem.SetTitle(item.Label)
		if item.Enabled {
			menuItem.Enable()
		} else {
			menuItem.Disable()
		}
	}
	if model.Pairing == nil {
		p.pairing.SetTitle("No pairing is pending")
		p.pairCode.SetTitle("")
		p.confirm.Disable()
		return
	}
	p.pairing.SetTitle("Pairing: " + model.Pairing.DeviceName)
	p.pairCode.SetTitle("Code: " + model.Pairing.Code)
	p.confirm.Enable()
}

func iconFor(icon tray.Icon) []byte {
	if image := icons[icon]; len(image) > 0 {
		return image
	}
	return icons[tray.IconNeedsAction]
}

func statusIcon(fill color.RGBA) []byte {
	canvas := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil
	}
	return output.Bytes()
}
