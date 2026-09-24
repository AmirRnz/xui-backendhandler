package panelurl

import "testing"

func TestValidatePanelURLTransportPolicy(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "remote https hostname", url: "https://panel.example.test", want: true},
		{name: "remote https with panel path", url: "https://panel.example.test/prefix", want: true},
		{name: "https loopback", url: "https://127.0.0.1:8443", want: true},
		{name: "http localhost", url: "http://localhost:8080", want: true},
		{name: "http ipv4 loopback", url: "http://127.0.0.1:8080", want: true},
		{name: "http ipv4 loopback range", url: "http://127.22.0.4:8080", want: true},
		{name: "http ipv6 loopback", url: "http://[::1]:8080", want: true},
		{name: "remote http hostname", url: "http://panel.example.test", want: false},
		{name: "remote http ipv4", url: "http://192.0.2.5:8080", want: false},
		{name: "remote http ipv6", url: "http://[2001:db8::1]:8080", want: false},
		{name: "localhost suffix is remote", url: "http://localhost.example.test", want: false},
		{name: "unsupported scheme", url: "ftp://panel.example.test", want: false},
		{name: "missing host", url: "https:///panel", want: false},
		{name: "embedded credentials", url: "https://user:password@panel.example.test", want: false},
		{name: "query", url: "https://panel.example.test?token=secret", want: false},
		{name: "fragment", url: "https://panel.example.test#panel", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.url)
			if (err == nil) != tt.want {
				t.Fatalf("Validate(%q) error = %v, want valid=%t", tt.url, err, tt.want)
			}
		})
	}
}
