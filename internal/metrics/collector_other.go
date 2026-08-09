//go:build !darwin && !windows

package metrics

import (
	"context"
	"errors"
	"time"
)

type otherSampler struct{}

func newPlatformSampler(int) PlatformSampler { return otherSampler{} }

func (otherSampler) Sample(context.Context, time.Time, bool) PlatformSample { return PlatformSample{} }

type unavailableDockerProbe struct{}

func newLocalDockerProbe() LocalDockerProbe { return unavailableDockerProbe{} }

func (unavailableDockerProbe) Running(context.Context) (bool, error) {
	return false, errors.New("local Docker state is unavailable")
}
