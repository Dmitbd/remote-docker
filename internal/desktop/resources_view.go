package desktop

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/Dmitbd/remote-docker/internal/metrics"
)

func (a *Application) resourcesBody() *fyne.Container {
	roles := ResourceRoleLabels()
	sample := a.resources
	title := widget.NewLabel("Нагрузка по устройствам")
	title.TextStyle = fyne.TextStyle{Bold: true}
	if sample.At.IsZero() {
		return container.NewVBox(title, widget.NewLabel("Измерения появятся через несколько секунд."))
	}

	localDocker := "Docker на Mac: состояние недоступно"
	if sample.LocalDockerEngine.Available {
		if sample.LocalDockerEngine.Running {
			localDocker = "Docker на Mac: запущен, но Remote Docker его не использует"
		} else {
			localDocker = "Docker на Mac: не запущен"
		}
	}
	localDockerLabel := widget.NewLabel(localDocker)
	localDockerLabel.TextStyle = fyne.TextStyle{Bold: true}

	mac := widget.NewCard(roles.Mac, "Ресурсы этого приложения на Mac", container.NewVBox(
		widget.NewLabel("CPU: "+cpuText(sample.MacRemoteDocker.CPUPercent)),
		widget.NewLabel("RAM: "+bytesText(sample.MacRemoteDocker.MemoryBytes)),
		localDockerLabel,
	))
	windows := widget.NewCard(roles.Windows, "Docker Engine и контейнеры находятся на Windows", container.NewVBox(
		widget.NewLabel("Remote Docker CPU: "+cpuText(sample.WindowsRemoteDocker.CPUPercent)),
		widget.NewLabel("Remote Docker RAM: "+bytesText(sample.WindowsRemoteDocker.MemoryBytes)),
		widget.NewLabel("Managed WSL CPU: "+cpuText(sample.WindowsManagedWSL.CPUPercent)),
		widget.NewLabel("Managed WSL RAM: "+bytesText(sample.WindowsManagedWSL.MemoryBytes)),
		widget.NewLabel("Контейнеры: "+intText(sample.DockerContainers)),
		widget.NewLabel("Данные Docker: "+bytesText(sample.ManagedDiskBytes)),
		widget.NewLabel("Синхронизация: "+rateText(sample.SyncNetwork)),
	))
	return container.NewVBox(title, mac, windows)
}

func cpuText(metric metrics.Metric[float64]) string {
	if !metric.Available {
		return unavailableText(metric.Reason)
	}
	return fmt.Sprintf("%.1f%%", metric.Value)
}

func bytesText(metric metrics.Metric[uint64]) string {
	if !metric.Available {
		return unavailableText(metric.Reason)
	}
	return humanBytes(float64(metric.Value))
}

func intText(metric metrics.Metric[int]) string {
	if !metric.Available {
		return unavailableText(metric.Reason)
	}
	return fmt.Sprintf("%d", metric.Value)
}

func rateText(rate metrics.Rate) string {
	if !rate.Available {
		return unavailableText(rate.Reason)
	}
	return humanBytes(rate.BytesPerSecond) + "/с"
}

func unavailableText(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "недоступно"
	}
	return "недоступно — " + reason
}

func humanBytes(value float64) string {
	units := []string{"Б", "КБ", "МБ", "ГБ", "ТБ"}
	index := 0
	for value >= 1024 && index < len(units)-1 {
		value /= 1024
		index++
	}
	if index == 0 {
		return fmt.Sprintf("%.0f %s", value, units[index])
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}
