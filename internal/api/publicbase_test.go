package api

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

// A phone on the LAN must never be handed a 127.0.0.1 link — that address
// resolves to the phone itself. When the operator has not declared a public
// URL, links follow the host the request actually arrived on.
func TestPublicBase(t *testing.T) {
	cases := []struct {
		name      string
		publicURL string
		servePort string
		host      string
		tls       bool
		want      string
	}{
		{
			name: "loopback stays loopback", servePort: "8081",
			host: "127.0.0.1:8080", want: "http://127.0.0.1:8081",
		},
		{
			name: "LAN host keeps the LAN address", servePort: "8081",
			host: "192.168.1.10:8080", want: "http://192.168.1.10:8081",
		},
		{
			name: "tailnet name survives", servePort: "8081",
			host: "creo.tail1234.ts.net:8080", want: "http://creo.tail1234.ts.net:8081",
		},
		{
			name: "https when the request was TLS", servePort: "8081",
			host: "creo.tail1234.ts.net", tls: true, want: "https://creo.tail1234.ts.net:8081",
		},
		{
			name: "an explicit public URL always wins", publicURL: "https://sites.example.com",
			servePort: "8081", host: "192.168.1.10:8080", want: "https://sites.example.com",
		},
		{
			name: "IPv6 host is bracketed", servePort: "8081",
			host: "[fd7a:115c::1]:8080", want: "http://[fd7a:115c::1]:8081",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &API{Deps{PublicURL: c.publicURL, ServePort: c.servePort}}
			r := httptest.NewRequest("GET", "/v1/projects/p1/preview", nil)
			r.Host = c.host
			if c.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := a.publicBase(r); got != c.want {
				t.Fatalf("publicBase = %q, want %q", got, c.want)
			}
		})
	}
}
