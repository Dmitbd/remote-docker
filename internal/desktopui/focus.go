package desktopui

import "github.com/Dmitbd/remote-docker/internal/localapi"

func FocusEndpoint() (string, error) {
	endpoint, err := localapi.DefaultEndpoint()
	if err != nil {
		return "", err
	}
	return endpoint + ".ui", nil
}
