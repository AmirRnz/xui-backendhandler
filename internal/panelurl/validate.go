// Package panelurl validates configured 3x-ui panel endpoints.
package panelurl

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Validate requires TLS for remotely addressed panels. Plain HTTP is limited
// to loopback endpoints used by isolated local test stacks.
func Validate(raw string) error {
	value := strings.TrimSpace(raw)
	u, err := url.ParseRequestURI(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(value) > 500 {
		return fmt.Errorf("panel base_url must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("panel base_url must use HTTPS unless its host is loopback")
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || (ip.To4() != nil && ip.To4().IsLoopback()))
}
