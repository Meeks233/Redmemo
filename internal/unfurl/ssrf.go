package unfurl

import (
	"net"
	"net/url"
)

// isPublicHTTPURL reports whether raw is an http(s) URL whose host is safe to
// fetch server-side: a public DNS name or public IP. The unfurl URL is fully
// attacker-influenced (it comes from a pasted link, and /api/unfurl takes it as
// a query param), so this gate is the SSRF boundary — it rejects any literal or
// resolved private/loopback/link-local/unspecified address so a crafted link
// can't coerce a fetch of an internal service (Redis/Postgres on localhost,
// 169.254.169.254 cloud metadata, LAN HTTP, …). DNS-resolution failure is a
// rejection: we never fetch a host we can't vet.
func isPublicHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPublicIP(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return false
		}
	}
	return true
}

// isPublicIP reports whether ip is a globally-routable unicast address — not
// loopback, private (RFC1918 / ULA), link-local (incl. 169.254.169.254 metadata),
// unspecified, or multicast.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		return !(v4[0] == 0 || v4[0] == 10 ||
			(v4[0] == 172 && v4[1]&0xf0 == 16) ||
			(v4[0] == 192 && v4[1] == 168) ||
			(v4[0] == 169 && v4[1] == 254) ||
			v4[0] == 127)
	}
	return true
}
