package transport

import (
	"context"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

func NewSpoofedClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: NewSpoofedTransport(),
		Timeout:   timeout,
	}
}

func NewSpoofedTransport() *http.Transport {
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: 10 * time.Second}
			rawConn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}

			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}

			uconn := utls.UClient(rawConn, &utls.Config{
				ServerName: host,
			}, utls.HelloCustom)

			spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
			if err != nil {
				rawConn.Close()
				return nil, err
			}

			// Replace ALPN to only advertise http/1.1.
			// HelloChrome_Auto includes h2 which Go's http.Transport
			// cannot handle, causing EOF.
			for i, ext := range spec.Extensions {
				if alpn, ok := ext.(*utls.ALPNExtension); ok {
					alpn.AlpnProtocols = []string{"http/1.1"}
					spec.Extensions[i] = alpn
					break
				}
			}

			if err := uconn.ApplyPreset(&spec); err != nil {
				rawConn.Close()
				return nil, err
			}

			if err := uconn.HandshakeContext(ctx); err != nil {
				rawConn.Close()
				return nil, err
			}

			return uconn, nil
		},
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
}
