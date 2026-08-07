package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/tray"
)

var icons = map[tray.Icon][]byte{
	tray.IconUnpaired:    statusPNG(color.RGBA{R: 128, G: 128, B: 128, A: 255}),
	tray.IconConnecting:  statusPNG(color.RGBA{R: 54, G: 123, B: 245, A: 255}),
	tray.IconStarting:    statusPNG(color.RGBA{R: 230, G: 152, B: 0, A: 255}),
	tray.IconSyncing:     statusPNG(color.RGBA{R: 119, G: 73, B: 204, A: 255}),
	tray.IconReady:       statusPNG(color.RGBA{R: 37, G: 145, B: 70, A: 255}),
	tray.IconDegraded:    statusPNG(color.RGBA{R: 206, G: 99, B: 0, A: 255}),
	tray.IconNeedsAction: statusPNG(color.RGBA{R: 190, G: 43, B: 43, A: 255}),
}

func main() {
	presentation := newPresentation(nativeDirectoryPicker{})
	systray.Run(presentation.ready, nil)
}

type directoryPicker interface {
	Choose(context.Context) (string, error)
}

type presentation struct {
	controller *tray.Controller
	picker     directoryPicker

	mu         sync.Mutex
	items      map[tray.Action]*systray.MenuItem
	pairing    *systray.MenuItem
	pairCode   *systray.MenuItem
	confirm    *systray.MenuItem
	candidates []*systray.MenuItem
}

func newPresentation(picker directoryPicker) *presentation {
	controller := tray.NewController(localapi.Client{})
	presentation := &presentation{
		controller: controller,
		picker:     picker,
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	switch action {
	case tray.ActionPair:
		_, _ = p.controller.DiscoverPairingCandidates(ctx)
	case tray.ActionOpenStatus:
		_, _ = p.controller.OpenStatus(ctx)
	case tray.ActionAddWorkspace:
		if p.picker == nil {
			return
		}
		path, err := p.picker.Choose(ctx)
		if err != nil {
			return
		}
		_, _ = p.controller.AddWorkspace(ctx, path)
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
	p.updateCandidates(model.Candidates)
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
		return platformIcon(image)
	}
	return platformIcon(icons[tray.IconNeedsAction])
}

func statusPNG(fill color.RGBA) []byte {
	canvas := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil
	}
	return output.Bytes()
}

func icoFromPNG(pngBytes []byte) []byte {
	if len(pngBytes) == 0 {
		return nil
	}
	icon := make([]byte, 22, 22+len(pngBytes))
	binary.LittleEndian.PutUint16(icon[2:4], 1)
	binary.LittleEndian.PutUint16(icon[4:6], 1)
	icon[6], icon[7] = 16, 16
	binary.LittleEndian.PutUint16(icon[10:12], 1)
	binary.LittleEndian.PutUint16(icon[12:14], 32)
	binary.LittleEndian.PutUint32(icon[14:18], uint32(len(pngBytes)))
	binary.LittleEndian.PutUint32(icon[18:22], 22)
	return append(icon, pngBytes...)
}

func (p *presentation) updateCandidates(candidates []localapi.PairingCandidate) {
	for _, item := range p.candidates {
		item.Remove()
	}
	p.candidates = nil
	parent := p.items[tray.ActionPair]
	if parent == nil {
		return
	}
	nameCounts := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		nameCounts[candidate.Name]++
	}
	for _, candidate := range candidates {
		candidate := candidate
		label := candidate.Name
		if nameCounts[candidate.Name] > 1 {
			label += " — " + shortCandidateID(candidate.ID)
		}
		if candidate.Unverified {
			label += " (verify code)"
		}
		item := parent.AddSubMenuItem(label, "Device name is unverified until you compare the pairing code")
		p.candidates = append(p.candidates, item)
		go func() {
			for range item.ClickedCh {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_, _ = p.controller.PairCandidate(ctx, candidate)
				}()
			}
		}()
	}
}

func shortCandidateID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
