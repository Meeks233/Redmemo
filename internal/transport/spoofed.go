package transport

import (
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/http2"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	tls "github.com/bogdanfinn/utls"
)

// NewSpoofedClient builds a tls-client HTTP client whose TLS ClientHello and
// HTTP/2 frame fingerprint both match the real Reddit Android app (OkHttp).
//
// The static profile is known-valid, so a construction failure is a
// programming error — it panics rather than threading an error through every
// call site. Extra options (e.g. WithNotFollowRedirects) are appended last so
// callers can override the defaults.
func NewSpoofedClient(timeout time.Duration, opts ...tls_client.HttpClientOption) tls_client.HttpClient {
	return newClientWithProfile(redditClientProfile(), timeout, opts...)
}

// NewMediaSpoofedClient builds a tls-client tuned for v.redd.it / i.redd.it CDN
// fetches. TLS ClientHello stays byte-identical to the API client so any
// fingerprint-driven CDN gating still passes, but the HTTP/2 flow-control
// windows are blown up by 16× so a single multi-MiB media stream — or a page
// full of concurrent video downloads sharing one h2 connection — never
// exhausts the shared connection-level window and gets RST_STREAM'd with
// FLOW_CONTROL_ERROR mid-body (the "1s then corrupt" symptom). The Akamai h2
// SETTINGS fingerprint diverges from the captured app profile, but the CDN
// origin (Fastly) does not fingerprint h2 SETTINGS the way oauth.reddit.com's
// Akamai edge does; verified against captures of the real app, which itself
// varies these windows by build.
func NewMediaSpoofedClient(timeout time.Duration, opts ...tls_client.HttpClientOption) tls_client.HttpClient {
	return newClientWithProfile(mediaClientProfile(), timeout, opts...)
}

func newClientWithProfile(profile profiles.ClientProfile, timeout time.Duration, opts ...tls_client.HttpClientOption) tls_client.HttpClient {
	base := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profile),
		tls_client.WithTimeoutSeconds(int(timeout.Seconds())),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), append(base, opts...)...)
	if err != nil {
		panic("transport: build spoofed client: " + err.Error())
	}
	return client
}

// redditClientProfile pairs our hand-audited ClientHello (carried verbatim by
// redditClientHelloSpec) with the OkHttp HTTP/2 fingerprint captured 2026-05:
//
//	Akamai h2: 4:16777216|16711681|0|m,p,a,s
//
// i.e. a single SETTINGS param (INITIAL_WINDOW_SIZE=16777216), a connection
// WINDOW_UPDATE increment of 16711681, no PRIORITY frames, and the
// method/path/authority/scheme pseudo-header order. These h2 values mirror the
// stock profiles.Okhttp4Android11 profile; only the ClientHello is ours.
func redditClientProfile() profiles.ClientProfile {
	return profiles.NewClientProfile(
		tls.ClientHelloID{
			Client:  "RedMemoRedditAndroid",
			Version: "1",
			SpecFactory: func() (tls.ClientHelloSpec, error) {
				return redditClientHelloSpec(), nil
			},
		},
		map[http2.SettingID]uint32{
			http2.SettingInitialWindowSize: 16777216,
		},
		[]http2.SettingID{
			http2.SettingInitialWindowSize,
		},
		[]string{":method", ":path", ":authority", ":scheme"},
		16711681,                // connectionFlow (WINDOW_UPDATE increment)
		nil,                     // priorities — no PRIORITY frames
		&http2.PriorityParam{},  // headerPriority — empty
		0,                       // streamID
		false,                   // allowHTTP
		nil, nil, 0, nil, false, // HTTP/3 — unused
	)
}

// mediaClientProfile reuses the API client's TLS ClientHello (same JA3/JA4)
// but inflates the HTTP/2 flow-control windows so v.redd.it CDN media streams
// don't asphyxiate at the 16 MiB connection ceiling.
//
// Akamai h2: 4:67108864|268435455|0|m,p,a,s
//
//   - SETTINGS INITIAL_WINDOW_SIZE = 64 MiB. fhttp/http2 mirrors this into
//     `cc.streamFlow` so a single Range response can carry up to 64 MiB before
//     a WINDOW_UPDATE is required (and auto-replenish fires every ~16 KiB).
//   - connectionFlow = (2^28-1) ≈ 256 MiB. The post-SETTINGS WINDOW_UPDATE the
//     client sends on the connection right after the preface, which sets the
//     ceiling for "bytes outstanding across ALL streams on this conn before
//     conn-level replenish". Auto-replenish triggers at half (~128 MiB) — high
//     enough that a viewport full of muxable videos won't drain it.
func mediaClientProfile() profiles.ClientProfile {
	return profiles.NewClientProfile(
		tls.ClientHelloID{
			Client:  "RedMemoRedditAndroidMedia",
			Version: "1",
			SpecFactory: func() (tls.ClientHelloSpec, error) {
				return redditClientHelloSpec(), nil
			},
		},
		map[http2.SettingID]uint32{
			http2.SettingInitialWindowSize: 67108864, // 64 MiB
		},
		[]http2.SettingID{
			http2.SettingInitialWindowSize,
		},
		[]string{":method", ":path", ":authority", ":scheme"},
		268435455, // connectionFlow — 2^28 - 1 (~256 MiB)
		nil,
		&http2.PriorityParam{},
		0,
		false,
		nil, nil, 0, nil, false,
	)
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
//
// Ported verbatim from the refraction-networking/utls spec to the
// bogdanfinn/utls fork; JA3 (1d714db2228763eab228fc28ce7f8e4f) and JA4
// (t13d1513h2) must stay byte-identical — verify with cmd/tlsprobe.
func redditClientHelloSpec() tls.ClientHelloSpec {
	return tls.ClientHelloSpec{
		TLSVersMin: tls.VersionTLS12,
		TLSVersMax: tls.VersionTLS13,
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,                  // 0x1301
			tls.TLS_AES_256_GCM_SHA384,                  // 0x1302
			tls.TLS_CHACHA20_POLY1305_SHA256,            // 0x1303
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, // 0xc02b
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,   // 0xc02f
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, // 0xc02c
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,   // 0xc030
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,  // 0xcca9
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,    // 0xcca8
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,      // 0xc013
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,      // 0xc014
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,         // 0x009c
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,         // 0x009d
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,            // 0x002f
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,            // 0x0035
		},
		CompressionMethods: []byte{0x00}, // null
		Extensions: []tls.TLSExtension{
			&tls.SNIExtension{},                  // 0  server_name
			&tls.ExtendedMasterSecretExtension{}, // 23 extended_master_secret
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient}, // 0xff01
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{
				tls.X25519,    // 0x1d
				tls.CurveP256, // 0x17
				tls.CurveP384, // 0x18
			}}, // 10 supported_groups — no GREASE, no post-quantum
			&tls.SupportedPointsExtension{SupportedPoints: []byte{0x00}},  // 11 ec_point_formats
			&tls.SessionTicketExtension{},                                 // 35 session_ticket
			&tls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}}, // 16 ALPN
			&tls.StatusRequestExtension{},                                 // 5  status_request
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
				tls.ECDSAWithP256AndSHA256, // 0x0403
				tls.PSSWithSHA256,          // 0x0804
				tls.PKCS1WithSHA256,        // 0x0401
				tls.ECDSAWithP384AndSHA384, // 0x0503
				tls.PSSWithSHA384,          // 0x0805
				tls.PKCS1WithSHA384,        // 0x0501
				tls.PSSWithSHA512,          // 0x0806
				tls.PKCS1WithSHA512,        // 0x0601
				tls.PKCS1WithSHA1,          // 0x0201
			}}, // 13 signature_algorithms
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{
				{Group: tls.X25519}, // 51 key_share — x25519 only
			}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}}, // 45 psk_key_exchange_modes
			&tls.SupportedVersionsExtension{Versions: []uint16{
				tls.VersionTLS13,
				tls.VersionTLS12,
			}}, // 43 supported_versions
			// padding(21) — emitted only when ClientHello length lands in a
			// size band, exactly mirroring the conditional behavior observed
			// in the capture.
			&tls.UtlsPaddingExtension{GetPaddingLen: tls.BoringPaddingStyle},
		},
	}
}

// appHeaderOrder is the canonical request-header order for the spoofed Reddit
// Android client. OkHttp serializes user-set headers in a stable order; this
// list reproduces it so headers built from a Go map (TokenInfo.Headers,
// SpoofIdentity.Headers — iterated in nondeterministic order) are still sent
// deterministically. Names are lowercase: tls-client matches the order list
// case-insensitively. Headers absent from a given request are simply skipped;
// any header not listed sorts after the listed ones.
//
// Source: inferred from the spoof header set in internal/oauth/spoof.go plus
// the OkHttp request-builder order; refine against a fresh app capture.
var appHeaderOrder = []string{
	"host",
	"authorization",
	"user-agent",
	"x-reddit-loid",
	"x-reddit-session",
	"x-reddit-retry",
	"x-reddit-compression",
	"x-reddit-qos",
	"x-reddit-media-codecs",
	"client-vendor-id",
	"x-reddit-device-id",
	"content-type",
	"accept",
	"accept-language",
	"accept-encoding",
	"range",
	"if-modified-since",
	"if-none-match",
	"cache-control",
	"cookie",
	"sec-gpc",
	"origin",
	"connection",
}

// AppHeaderOrder returns a copy of the canonical Reddit-app header order.
func AppHeaderOrder() []string {
	out := make([]string, len(appHeaderOrder))
	copy(out, appHeaderOrder)
	return out
}

// ApplyHeaderOrder pins an outbound request's header and pseudo-header order to
// the canonical Reddit-app layout via fhttp's HeaderOrderKey / PHeaderOrderKey.
// Call it after all headers have been set.
func ApplyHeaderOrder(req *http.Request) {
	req.Header[http.HeaderOrderKey] = AppHeaderOrder()
	req.Header[http.PHeaderOrderKey] = []string{":method", ":path", ":authority", ":scheme"}
}
