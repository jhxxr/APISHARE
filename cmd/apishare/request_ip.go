package main

import (
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
)

const requestIPPolicyKey = "requestIPPolicy"

type requestIPPolicy struct {
	trustForwarded bool
	headers        []string
}

func configureRequestIP(r *gin.Engine) {
	proxies := strings.FieldsFunc(os.Getenv("TRUSTED_PROXIES"), func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	if err := r.SetTrustedProxies(proxies); err != nil {
		log.Printf("[request-ip] invalid TRUSTED_PROXIES: %v; disabling trusted proxies", err)
		_ = r.SetTrustedProxies(nil)
	}

	policy := requestIPPolicy{trustForwarded: true}
	if value := strings.TrimSpace(os.Getenv("LOG_TRUST_FORWARDED_IP")); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			log.Print("[request-ip] invalid LOG_TRUST_FORWARDED_IP; using trusted-proxy mode")
		}
		policy.trustForwarded = err == nil && enabled
	}
	for _, value := range strings.Split(os.Getenv("LOG_FORWARDED_IP_HEADERS"), ",") {
		header := strings.TrimSpace(value)
		if header == "" {
			continue
		}
		if !httpguts.ValidHeaderFieldName(header) {
			log.Print("[request-ip] ignoring invalid LOG_FORWARDED_IP_HEADERS entry")
			continue
		}
		policy.headers = append(policy.headers, http.CanonicalHeaderKey(header))
	}
	policy.headers = append(policy.headers, "CF-Connecting-IP", "True-Client-IP", "X-Real-IP", "X-Forwarded-For")
	r.Use(func(c *gin.Context) {
		c.Set(requestIPPolicyKey, policy)
		c.Next()
	})
}

func parseCleanIP(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	if idx := strings.IndexByte(s, '%'); idx >= 0 {
		s = s[:idx]
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !addr.IsValid() {
		return ""
	}
	return addr.Unmap().String()
}

func extractRequestIP(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if value, exists := c.Get(requestIPPolicyKey); exists {
		if policy, ok := value.(requestIPPolicy); ok && policy.trustForwarded {
			var fallback string
			for _, header := range policy.headers {
				for _, value := range c.Request.Header.Values(header) {
					for _, candidate := range strings.Split(value, ",") {
						ip := parseCleanIP(candidate)
						addr, err := netip.ParseAddr(ip)
						if err != nil || addr.IsUnspecified() || addr.IsMulticast() {
							continue
						}
						if addr.IsGlobalUnicast() && !addr.IsPrivate() {
							return ip
						}
						if fallback == "" {
							fallback = ip
						}
					}
				}
			}
			if fallback != "" {
				return fallback
			}
		}
	}
	// Compatibility headers are log metadata only. Security-sensitive callers
	// continue using c.ClientIP(), governed by TRUSTED_PROXIES.
	return parseCleanIP(c.ClientIP())
}
