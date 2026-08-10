package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"
)

const routeHandshakeTimeout = 5 * time.Second

type RoutedListeners struct {
	Pairing net.Listener
	Tunnel  net.Listener
}

func RoutingTLSConfig(pairingConfig, tunnelConfig *tls.Config) (*tls.Config, error) {
	if pairingConfig == nil {
		return nil, errors.New("pairing TLS configuration is required")
	}
	config := pairingConfig.Clone()
	config.NextProtos = []string{PairingALPN, TunnelALPN}
	config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		for _, protocol := range hello.SupportedProtos {
			if protocol == TunnelALPN {
				if tunnelConfig == nil {
					return nil, errors.New("authenticated tunnel is not available")
				}
				return tunnelConfig.Clone(), nil
			}
		}
		for _, protocol := range hello.SupportedProtos {
			if protocol == PairingALPN {
				return pairingConfig.Clone(), nil
			}
		}
		return nil, errors.New("unsupported Remote Docker ALPN")
	}
	return config, nil
}

func RouteTLS(ctx context.Context, base net.Listener, config *tls.Config) (RoutedListeners, error) {
	if ctx == nil || base == nil || config == nil {
		return RoutedListeners{}, errors.New("TLS router dependencies are incomplete")
	}
	pairing := newRoutedListener(base.Addr())
	tunnel := newRoutedListener(base.Addr())
	routerCtx, cancel := context.WithCancel(ctx)
	closeAll := func() {
		cancel()
		_ = base.Close()
		_ = pairing.Close()
		_ = tunnel.Close()
	}
	go func() {
		<-routerCtx.Done()
		closeAll()
	}()
	go func() {
		defer closeAll()
		var handshakes sync.WaitGroup
		defer handshakes.Wait()
		for {
			connection, err := base.Accept()
			if err != nil {
				return
			}
			handshakes.Add(1)
			go func(raw net.Conn) {
				defer handshakes.Done()
				tlsConnection := tls.Server(raw, config.Clone())
				handshakeCtx, handshakeCancel := context.WithTimeout(routerCtx, routeHandshakeTimeout)
				err := tlsConnection.HandshakeContext(handshakeCtx)
				handshakeCancel()
				if err != nil {
					_ = raw.Close()
					return
				}
				var destination *routedListener
				switch tlsConnection.ConnectionState().NegotiatedProtocol {
				case PairingALPN:
					destination = pairing
				case TunnelALPN:
					destination = tunnel
				default:
					_ = tlsConnection.Close()
					return
				}
				if !destination.deliver(routerCtx, tlsConnection) {
					_ = tlsConnection.Close()
				}
			}(connection)
		}
	}()
	return RoutedListeners{Pairing: pairing, Tunnel: tunnel}, nil
}

type routedListener struct {
	address net.Addr
	accept  chan net.Conn
	closed  chan struct{}
	once    sync.Once
}

func newRoutedListener(address net.Addr) *routedListener {
	return &routedListener{address: address, accept: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *routedListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case connection := <-l.accept:
		return connection, nil
	}
}

func (l *routedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *routedListener) Addr() net.Addr { return l.address }

func (l *routedListener) deliver(ctx context.Context, connection net.Conn) bool {
	select {
	case <-ctx.Done():
		return false
	case <-l.closed:
		return false
	case l.accept <- connection:
		return true
	}
}
