package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

func loadDotEnv() error {
	err := godotenv.Load()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

type ctxLoggerKey struct{}

func withLogger(ctx context.Context, l *zap.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxLoggerKey{}, l)
}

func loggerFromCtx(ctx context.Context) *zap.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxLoggerKey{}).(*zap.Logger); ok && l != nil {
			return l
		}
	}
	if logger != nil {
		return logger
	}
	return zap.NewNop()
}

func getNasaTime() time.Time {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil || loc == nil {
		return time.Now().UTC()
	}
	return time.Now().In(loc)
}

func getString(v interface{}) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getenvFloat64(key string, fallback float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return n
}

func buildETag(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte("|"))
	}
	return "\"" + hex.EncodeToString(h.Sum(nil)) + "\""
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

var (
	trustedProxyMu    sync.RWMutex
	trustedProxyIPs   = map[string]struct{}{}
	trustedProxyCIDRs []*net.IPNet
	trustAllProxies   bool
)

func setTrustedProxiesForRealIP(proxies []string) {
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

func realIP(r *http.Request) string {
	remoteIP := splitClientAndPort(r.RemoteAddr)
	if !isTrustedProxyIP(remoteIP) {
		return remoteIP
	}

	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		chain := forwardedForChain(xff)
		if len(chain) > 0 {
			// Walk right-to-left and return the first untrusted hop.
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

func baseURL(r *http.Request) string {
	scheme := detectScheme(r)
	host := r.Host
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		host = firstCSV(xfh)
	}
	return scheme + "://" + host
}
