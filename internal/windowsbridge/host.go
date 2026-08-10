package windowsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"sync"
	"time"
	"unicode/utf16"
)

const (
	SSHBridgePort         = 49222
	SyncthingBridgePort   = 49220
	privateIPv4PowerShell = `$ErrorActionPreference='Stop'; $addresses=@(Get-NetIPConfiguration | Where-Object { $_.NetProfile -and [int]$_.NetProfile.NetworkCategory -eq 1 } | ForEach-Object { $_.IPv4Address.IPAddress } | Where-Object { $_ }); ConvertTo-Json -Compress -InputObject @($addresses)`
)

var ErrNoPrivateListenAddress = errors.New("no Private-profile IPv4 listen address is available")

// AddressProvider returns only addresses that currently belong to a Windows
// network interface whose profile is Private.
type AddressProvider interface {
	Addresses(context.Context) ([]net.IP, error)
}

// PowerShellPrivateNetwork reads Windows network profile state through one
// fixed, non-interactive command. Results are cached because Proxy checks the
// profile while it is accepting connections.
type PowerShellPrivateNetwork struct {
	Runner   OutputRunner
	CacheTTL time.Duration
	Now      func() time.Time

	mu        sync.Mutex
	cached    []net.IP
	cachedAt  time.Time
	cachedErr error
}

func (p *PowerShellPrivateNetwork) Addresses(ctx context.Context) ([]net.IP, error) {
	if p == nil {
		return nil, ErrProfileUnavailable
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	ttl := p.CacheTTL
	if ttl <= 0 {
		ttl = 2 * time.Second
	}
	if !p.cachedAt.IsZero() && now.Sub(p.cachedAt) < ttl {
		return cloneIPs(p.cached), p.cachedErr
	}
	runner := p.Runner
	if runner == nil {
		runner = commandOutputRunner{}
	}
	output, err := runner.Output(ctx,
		"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", privateIPv4PowerShell,
	)
	if err != nil {
		p.cached, p.cachedAt, p.cachedErr = nil, now, ErrProfileUnavailable
		return nil, p.cachedErr
	}
	addresses, err := decodePrivateIPv4(output)
	if err != nil {
		p.cached, p.cachedAt, p.cachedErr = nil, now, err
		return nil, err
	}
	p.cached, p.cachedAt, p.cachedErr = addresses, now, nil
	return cloneIPs(addresses), nil
}

func decodePrivateIPv4(output []byte) ([]net.IP, error) {
	output = decodePowerShellText(output)
	var values []string
	if err := json.Unmarshal(bytes.TrimSpace(output), &values); err != nil {
		return nil, ErrProfileUnavailable
	}
	unique := make(map[string]net.IP, len(values))
	for _, value := range values {
		address := net.ParseIP(value)
		if address == nil || address.To4() == nil || !address.IsPrivate() {
			continue
		}
		address = append(net.IP(nil), address.To4()...)
		unique[address.String()] = address
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	addresses := make([]net.IP, 0, len(keys))
	for _, key := range keys {
		addresses = append(addresses, unique[key])
	}
	if len(addresses) == 0 {
		return nil, ErrNoPrivateListenAddress
	}
	return addresses, nil
}

func decodePowerShellText(output []byte) []byte {
	if len(output) < 2 {
		return output
	}
	start := 0
	if output[0] == 0xff && output[1] == 0xfe {
		start = 2
	} else if !looksLikeUTF16LE(output) {
		return output
	}
	units := make([]uint16, 0, (len(output)-start)/2)
	for index := start; index+1 < len(output); index += 2 {
		units = append(units, uint16(output[index])|uint16(output[index+1])<<8)
	}
	return []byte(string(utf16.Decode(units)))
}

func looksLikeUTF16LE(output []byte) bool {
	if len(output) < 4 || len(output)%2 != 0 {
		return false
	}
	nulls := 0
	for index := 1; index < len(output); index += 2 {
		if output[index] == 0 {
			nulls++
		}
	}
	return nulls*4 >= len(output)
}

func cloneIPs(addresses []net.IP) []net.IP {
	result := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, append(net.IP(nil), address...))
	}
	return result
}

// Host is retained as a lifecycle-compatible fixed WSL service dialer. It
// deliberately owns no LAN listeners; TCP 49221 is owned by the TLS router.
type Host struct {
	Addresses AddressProvider
	Resolver  AddressResolver
	Dialer    Dialer
	Listen    func(string, string) (net.Listener, error)
}

func NewHost(addresses AddressProvider, resolver AddressResolver, dialer Dialer) (*Host, error) {
	if addresses == nil {
		return nil, ErrProfileUnavailable
	}
	if resolver == nil {
		return nil, ErrResolverUnavailable
	}
	if dialer == nil {
		return nil, ErrDialerUnavailable
	}
	return &Host{Addresses: addresses, Resolver: resolver, Dialer: dialer, Listen: net.Listen}, nil
}

func NewProductionHost() (*Host, error) {
	return NewHost(
		&PowerShellPrivateNetwork{},
		WSLResolver{Distro: managedDistroName},
		&net.Dialer{},
	)
}

func (h *Host) Run(ctx context.Context, retryDelay time.Duration) error {
	if h == nil || h.Resolver == nil || h.Dialer == nil {
		return errors.New("Windows bridge host is incomplete")
	}
	<-ctx.Done()
	return ctx.Err()
}

func (h *Host) serve(ctx context.Context) error {
	if h == nil || h.Addresses == nil || h.Resolver == nil || h.Dialer == nil {
		return errors.New("Windows bridge host is incomplete")
	}
	<-ctx.Done()
	return ctx.Err()
}

type boundAddressProfile struct {
	addresses AddressProvider
	address   net.IP
}

func (p boundAddressProfile) Private(ctx context.Context) (bool, error) {
	addresses, err := p.addresses.Addresses(ctx)
	if err != nil {
		return false, err
	}
	for _, address := range addresses {
		if address.Equal(p.address) {
			return true, nil
		}
	}
	return false, nil
}

var _ interface {
	Run(context.Context, time.Duration) error
} = (*Host)(nil)
