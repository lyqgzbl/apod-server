package httputil

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

// GetNasaTime returns the current time in NASA's Eastern Time zone.
func GetNasaTime() time.Time {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil || loc == nil {
		return time.Now().UTC()
	}
	return time.Now().In(loc)
}

// IsToday reports whether the given date string matches today's date in NASA time.
func IsToday(date string) bool {
	return date == GetNasaTime().Format("2006-01-02")
}

// IsValidISODate reports whether the string is a valid YYYY-MM-DD date.
func IsValidISODate(date string) bool {
	if date == "" {
		return false
	}
	_, err := time.Parse("2006-01-02", date)
	return err == nil
}

// BuildETag creates an ETag from the given parts using SHA-256.
func BuildETag(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte("|"))
	}
	return "\"" + hex.EncodeToString(h.Sum(nil)) + "\""
}

// --- Trusted Proxy & Real IP ---

var (
	trustedProxyMu    sync.RWMutex
	trustedProxyIPs   = map[string]struct{}{}
	trustedProxyCIDRs []*net.IPNet
	trustAllProxies   bool
)

// SetTrustedProxiesForRealIP configures trusted proxy IPs/CIDRs for RealIP resolution.
func SetTrustedProxiesForRealIP(proxies []string) {
	ips := make(map[string]struct{}, len(proxies))
	cidrs := make([]*net.IPNet, 0, len(proxies))
	all := false

	for _, raw := range proxies {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if v == "*" {
			all = true
			continue
		}
		if strings.Contains(v, "/") {
			if _, ipnet, err := net.ParseCIDR(v); err == nil && ipnet != nil {
				cidrs = append(cidrs, ipnet)
			}
			continue
		}
		if ip := net.ParseIP(v); ip != nil {
			ips[ip.String()] = struct{}{}
		}
	}

	trustedProxyMu.Lock()
	trustedProxyIPs = ips
	trustedProxyCIDRs = cidrs
	trustAllProxies = all
	trustedProxyMu.Unlock()
}

func isTrustedProxyIP(ipStr string) bool {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return false
	}
	trustedProxyMu.RLock()
	defer trustedProxyMu.RUnlock()
	if trustAllProxies {
		return true
	}
	if _, ok := trustedProxyIPs[ip.String()]; ok {
		return true
	}
	for _, ipnet := range trustedProxyCIDRs {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// RealIP extracts the real client IP from the request, respecting trusted proxies.
func RealIP(r *http.Request) string {
	remoteIP := splitClientAndPort(r.RemoteAddr)
	if !isTrustedProxyIP(remoteIP) {
		return remoteIP
	}

	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		chain := forwardedForChain(xff)
		if len(chain) > 0 {
			for i := len(chain) - 1; i >= 0; i-- {
				if !isTrustedProxyIP(chain[i]) {
					return chain[i]
				}
			}
			return chain[0]
		}
	}

	if rip := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(rip) != nil {
		return rip
	}

	return remoteIP
}

// BaseURL returns the base URL (scheme + host) from the request.
func BaseURL(r *http.Request) string {
	scheme := detectScheme(r)
	host := r.Host
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		host = firstCSV(xfh)
	}
	return scheme + "://" + host
}

// --- internal helpers ---

func splitClientAndPort(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func forwardedForChain(xff string) []string {
	parts := strings.Split(xff, ",")
	ips := make([]string, 0, len(parts))
	for _, part := range parts {
		ip := strings.TrimSpace(part)
		if net.ParseIP(ip) != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

func firstCSV(v string) string {
	if v == "" {
		return ""
	}
	parts := strings.Split(v, ",")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func forwardedProto(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	entry := firstCSV(v)
	for _, part := range strings.Split(entry, ";") {
		part = strings.TrimSpace(part)
		if len(part) < 6 || !strings.HasPrefix(strings.ToLower(part), "proto=") {
			continue
		}
		proto := strings.Trim(strings.TrimSpace(part[6:]), "\"")
		return strings.ToLower(proto)
	}
	return ""
}

func detectScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if p := strings.ToLower(firstCSV(r.Header.Get("X-Forwarded-Proto"))); p == "https" || p == "http" {
		return p
	}
	if p := forwardedProto(r.Header.Get("Forwarded")); p == "https" || p == "http" {
		return p
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Ssl")), "on") {
		return "https"
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Front-End-Https")), "on") {
		return "https"
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Url-Scheme")), "https") {
		return "https"
	}
	if strings.Contains(strings.ToLower(r.Header.Get("CF-Visitor")), "\"scheme\":\"https\"") {
		return "https"
	}
	return "http"
}
