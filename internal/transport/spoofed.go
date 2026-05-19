package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

func NewSpoofedClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: NewSpoofedTransport(),
		Timeout:   timeout,
	}
}

// redditClientHelloSpec mirrors the real Reddit Android app TLS stack
// (Android Conscrypt), captured 2026-05-19 — see tls-fingerprint-audit.md.
//
// Deliberately subtractive vs. utls HelloChrome_Auto: no GREASE, no
// post-quantum key share, no Chrome-desktop-only extensions
// (compress_certificate/application_settings/encrypted_client_hello/SCT).
//
// Cipher set already matches Reddit (JA4 cipher hash 8daaf6152771); the
// extension set, supported_groups, and ALPN all match the capture — ALPN
// advertises h2, http/1.1 so JA4 ends in h2 like the real app.
func redditClientHelloSpec() utls.ClientHelloSpec {
	return utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{
			utls.TLS_AES_128_GCM_SHA256,                  // 0x1301
			utls.TLS_AES_256_GCM_SHA384,                  // 0x1302
			utls.TLS_CHACHA20_POLY1305_SHA256,            // 0x1303
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, // 0xc02b
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,   // 0xc02f
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, // 0xc02c
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,   // 0xc030
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,  // 0xcca9
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,    // 0xcca8
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,      // 0xc013
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,      // 0xc014
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,         // 0x009c
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,         // 0x009d
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,            // 0x002f
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,            // 0x0035
		},
		CompressionMethods: []byte{0x00}, // null
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},                  // 0  server_name
			&utls.ExtendedMasterSecretExtension{}, // 23 extended_master_secret
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient}, // 0xff01
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519,    // 0x1d
				utls.CurveP256, // 0x17
				utls.CurveP384, // 0x18
			}}, // 10 supported_groups — no GREASE, no post-quantum
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0x00}}, // 11 ec_point_formats
			&utls.SessionTicketExtension{},                                // 35 session_ticket
			&utls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}}, // 16 ALPN
			&utls.StatusRequestExtension{},                                // 5  status_request
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256, // 0x0403
				utls.PSSWithSHA256,          // 0x0804
				utls.PKCS1WithSHA256,        // 0x0401
				utls.ECDSAWithP384AndSHA384, // 0x0503
				utls.PSSWithSHA384,          // 0x0805
				utls.PKCS1WithSHA384,        // 0x0501
				utls.PSSWithSHA512,          // 0x0806
				utls.PKCS1WithSHA512,        // 0x0601
				utls.PKCS1WithSHA1,          // 0x0201
			}}, // 13 signature_algorithms
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519}, // 51 key_share — x25519 only
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}}, // 45 psk_key_exchange_modes
			&utls.SupportedVersionsExtension{Versions: []uint16{
				utls.VersionTLS13,
				utls.VersionTLS12,
			}}, // 43 supported_versions
			// padding(21) — emitted only when ClientHello length lands in a
			// size band, exactly mirroring the conditional behavior observed
			// in the capture.
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}
}

// errALPNHTTP1 is returned by the HTTP/2 transport's dialer when the server
// negotiated http/1.1; the wrapping RoundTripper catches it and reroutes the
// request through the HTTP/1.1 transport.
var errALPNHTTP1 = errors.New("alpn negotiated http/1.1")

func dialSpoofedTLS(ctx context.Context, addr string) (*utls.UConn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	uconn := utls.UClient(rawConn, &utls.Config{ServerName: host}, utls.HelloCustom)
	spec := redditClientHelloSpec()
	if err := uconn.ApplyPreset(&spec); err != nil {
		rawConn.Close()
		return nil, err
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	return uconn, nil
}

// spoofedRoundTripper owns parallel HTTP/1.1 and HTTP/2 transports that share
// the same uTLS dial. Per-host ALPN outcomes are cached so subsequent requests
// to an http/1.1-only host skip the failed-h2 round trip.
type spoofedRoundTripper struct {
	h1 *http.Transport
	h2 *http2.Transport

	mu   sync.RWMutex
	isH1 map[string]bool
}

func (s *spoofedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	s.mu.RLock()
	forceH1 := s.isH1[host]
	s.mu.RUnlock()
	if forceH1 {
		return s.h1.RoundTrip(req)
	}

	resp, err := s.h2.RoundTrip(req)
	if err != nil && errors.Is(err, errALPNHTTP1) {
		s.mu.Lock()
		s.isH1[host] = true
		s.mu.Unlock()
		return s.h1.RoundTrip(req)
	}
	return resp, err
}

// NewSpoofedTransport returns an http.RoundTripper that issues a Reddit-App
// shaped ClientHello (ALPN h2, http/1.1) and speaks whichever protocol the
// server negotiates.
func NewSpoofedTransport() http.RoundTripper {
	h1 := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialSpoofedTLS(ctx, addr)
		},
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	h2 := &http2.Transport{
		DialTLSContext: func(ctx context.Context, _, addr string, _ *tls.Config) (net.Conn, error) {
			conn, err := dialSpoofedTLS(ctx, addr)
			if err != nil {
				return nil, err
			}
			if conn.ConnectionState().NegotiatedProtocol != "h2" {
				conn.Close()
				return nil, errALPNHTTP1
			}
			return conn, nil
		},
		AllowHTTP:       false,
		IdleConnTimeout: 90 * time.Second,
	}

	return &spoofedRoundTripper{h1: h1, h2: h2, isH1: map[string]bool{}}
}
