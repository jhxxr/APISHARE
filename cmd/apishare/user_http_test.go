package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicUserURLValidation(t *testing.T) {
	for _, host := range []string{
		"localhost.", "api.localhost.", "api.local.", "api.internal.",
		"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "0.1.2.3",
		"[::1]", "[::ffff:127.0.0.1]", "[fe80::1%25eth0]", "[fc00::1]", "[ff02::1]",
	} {
		t.Run(host, func(t *testing.T) {
			if err := validatePublicUpstreamURL("http://"+host+":8080", false); err == nil {
				t.Fatal("accepted local/private upstream URL")
			}
			if err := validateUserProxyHost(strings.Trim(host, "[]")); err == nil {
				t.Fatal("accepted local/private proxy hostname")
			}
		})
	}
	for _, raw := range []string{"https://api.example.com/v1", "http://8.8.8.8", "https://[2001:4860:4860::8888]"} {
		if err := validatePublicUpstreamURL(raw, false); err != nil {
			t.Fatalf("rejected public URL %s: %v", raw, err)
		}
	}
}

func TestPublicUserTransportRejectsPrivateDNS(t *testing.T) {
	for _, addresses := range [][]net.IPAddr{
		{{IP: net.ParseIP("127.0.0.1")}},
		{{IP: net.ParseIP("10.0.0.1")}},
		{{IP: net.ParseIP("169.254.169.254")}},
		{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("::1")}},
		{},
	} {
		base := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			t.Error("blocked destination reached the dialer")
			return nil, errors.New("unexpected dial")
		}}
		guard := &publicUserTransport{base: base, lookup: func(context.Context, string) ([]net.IPAddr, error) { return addresses, nil }}
		req, _ := http.NewRequest(http.MethodGet, "http://public-name.example/v1/models", nil)
		if _, err := guard.RoundTrip(req); err == nil {
			t.Fatalf("accepted DNS answers %v", addresses)
		}
	}
}

func TestPublicUserTransportPinsDNSAndRejectsRebinding(t *testing.T) {
	var receivedHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	var lookupCalls, dialCalls atomic.Int32
	base := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCalls.Add(1)
		if address != "8.8.8.8:80" {
			t.Errorf("dialed unpinned address %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}}
	defer closePublicUserTransports(base)
	guard := &publicUserTransport{base: base, lookup: func(context.Context, string) ([]net.IPAddr, error) {
		if lookupCalls.Add(1) == 1 {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}}
	client := &http.Client{Transport: guard}
	req, _ := http.NewRequest(http.MethodGet, "http://public-name.example/v1/models", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if receivedHost != "public-name.example" || req.URL.Host != "public-name.example" || lookupCalls.Load() != 1 {
		t.Fatalf("Host/request changed or DNS was resolved twice: Host=%q URL=%s lookups=%d", receivedHost, req.URL, lookupCalls.Load())
	}
	if resp, err := client.Get(req.URL.String()); err == nil {
		resp.Body.Close()
		t.Fatal("rebound private address was accepted")
	}
	if dialCalls.Load() != 1 {
		t.Fatalf("rebound address reached dialer: calls=%d", dialCalls.Load())
	}
}

func TestPublicUserTransportPinsHTTPProxyDestination(t *testing.T) {
	for _, secureProxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secureProxy], func(t *testing.T) {
			var target, path string
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				target = r.URL.Host
				path = r.URL.RequestURI()
				io.WriteString(w, "ok")
			})
			var proxy *httptest.Server
			if secureProxy {
				proxy = httptest.NewTLSServer(handler)
			} else {
				proxy = httptest.NewServer(handler)
			}
			defer proxy.Close()
			proxyURL, _ := url.Parse("http://user-proxy.example:8080")
			if secureProxy {
				proxyURL.Scheme = "https"
			}
			base := &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != "8.8.4.4:8080" {
					t.Errorf("proxy was not pinned: %q", address)
				}
				return (&net.Dialer{}).DialContext(ctx, network, proxy.Listener.Addr().String())
			}}
			if secureProxy {
				base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local HTTPS proxy fixture.
			}
			defer closePublicUserTransports(base)
			guard := &publicUserTransport{base: base, userProxy: true, lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
				ip := "8.8.8.8"
				if host == proxyURL.Hostname() {
					ip = "8.8.4.4"
				}
				return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
			}}
			resp, err := (&http.Client{Transport: guard}).Get("http://target.example/v1/models?q=a%2Fb")
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if target != "8.8.8.8:80" || path != "/v1/models?q=a%2Fb" {
				t.Fatalf("proxy can re-resolve target, or path/query changed: target=%q path=%q", target, path)
			}
		})
	}
}

func TestPublicUserTransportRejectsPrivateProxyDNS(t *testing.T) {
	proxyURL, _ := url.Parse("http://user-proxy.example:8080")
	base := &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: func(context.Context, string, string) (net.Conn, error) {
		t.Error("private proxy reached the dialer")
		return nil, errors.New("unexpected dial")
	}}
	guard := &publicUserTransport{base: base, userProxy: true, lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
		ip := "8.8.8.8"
		if host == proxyURL.Hostname() {
			ip = "127.0.0.1"
		}
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}}
	req, _ := http.NewRequest(http.MethodGet, "http://public-name.example/v1/models", nil)
	if _, err := guard.RoundTrip(req); err == nil {
		t.Fatal("private custom proxy was accepted")
	}
}

func TestPublicUserHTTPClientDoesNotFollowRedirects(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	base := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, redirect.Listener.Addr().String())
	}}
	defer closePublicUserTransports(base)
	original := &http.Client{Transport: base, Timeout: time.Second}
	client, err := publicUserHTTPClient(original, false)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://8.8.8.8/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || targetCalls.Load() != 0 || original.CheckRedirect != nil {
		t.Fatalf("redirect policy was not isolated: status=%d targetCalls=%d", resp.StatusCode, targetCalls.Load())
	}
}

func TestPublicUserTransportPreservesTLSName(t *testing.T) {
	var sni, host string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni, host = r.TLS.ServerName, r.Host
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	base := srv.Client().Transport.(*http.Transport).Clone()
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Errorf("HTTPS destination was not pinned: %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	defer closePublicUserTransports(base)
	client, err := publicUserHTTPClient(&http.Client{Transport: base}, false)
	if err != nil {
		t.Fatal(err)
	}
	guard := client.Transport.(*publicUserTransport)
	guard.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	resp, err := client.Get("https://target.example.com/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if sni != "target.example.com" || host != "target.example.com" {
		t.Fatalf("TLS name/Host changed after IP pinning: SNI=%q Host=%q", sni, host)
	}
}

func TestPublicUserTransportPinsCONNECTAndVerifiesBothTLSHops(t *testing.T) {
	for _, secureProxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secureProxy], func(t *testing.T) {
			var originNames, proxyNames atomic.Value
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originNames.Store([]string{r.TLS.ServerName, r.Host})
				w.Header().Set("Connection", "close")
				io.WriteString(w, "ok")
			}))
			defer origin.Close()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxySNI := ""
				if r.TLS != nil {
					proxySNI = r.TLS.ServerName
				}
				proxyNames.Store([]string{r.Method, r.Host, proxySNI})
				upstream, err := net.Dial("tcp", origin.Listener.Addr().String())
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
				downstream, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					upstream.Close()
					return
				}
				defer downstream.Close()
				defer upstream.Close()
				fmt.Fprint(buffered, "HTTP/1.1 200 Connection Established\r\n\r\n")
				buffered.Flush()
				go func() {
					io.Copy(downstream, upstream)
					downstream.Close()
				}()
				io.Copy(upstream, buffered)
			})
			var proxy *httptest.Server
			if secureProxy {
				proxy = httptest.NewTLSServer(handler)
			} else {
				proxy = httptest.NewServer(handler)
			}
			defer proxy.Close()
			proxyURL, _ := url.Parse("http://proxy.example.com:8080")
			if secureProxy {
				proxyURL.Scheme = "https"
			}
			// httptest's certificate covers *.example.com. Keep certificate
			// verification enabled for both the proxy and the origin.
			base := origin.Client().Transport.(*http.Transport).Clone()
			base.Proxy = http.ProxyURL(proxyURL)
			base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != "8.8.4.4:8080" {
					t.Errorf("unvalidated proxy dial: %q", address)
				}
				return (&net.Dialer{}).DialContext(ctx, network, proxy.Listener.Addr().String())
			}
			defer closePublicUserTransports(base)
			guard := &publicUserTransport{base: base, userProxy: true, lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
				ip := "8.8.8.8"
				if host == "proxy.example.com" {
					ip = "8.8.4.4"
				}
				return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
			}}
			resp, err := (&http.Client{Transport: guard, Timeout: 5 * time.Second}).Get("https://target.example.com/v1/models")
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			names := originNames.Load().([]string)
			if names[0] != "target.example.com" || names[1] != "target.example.com" {
				t.Fatalf("origin TLS/Host was not preserved: %v", names)
			}
			names = proxyNames.Load().([]string)
			if names[0] != http.MethodConnect || names[1] != "8.8.8.8:443" {
				t.Fatalf("CONNECT target was not pinned: %v", names)
			}
			if secureProxy && names[2] != "proxy.example.com" {
				t.Fatalf("wrong proxy TLS name: %v", names)
			}
		})
	}
}

func TestPublicUserTransportPinsSOCKSDestination(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			conn, err := listener.Accept()
			if err != nil {
				return err
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			var greeting [2]byte
			if _, err := io.ReadFull(conn, greeting[:]); err != nil {
				return err
			}
			methods := make([]byte, int(greeting[1]))
			if _, err := io.ReadFull(conn, methods); err != nil {
				return err
			}
			if _, err := conn.Write([]byte{5, 0}); err != nil {
				return err
			}
			var request [10]byte
			if _, err := io.ReadFull(conn, request[:]); err != nil {
				return err
			}
			if request[0] != 5 || request[1] != 1 || request[3] != 1 ||
				!net.IP(request[4:8]).Equal(net.ParseIP("8.8.8.8")) || request[8] != 0 || request[9] != 80 {
				return fmt.Errorf("SOCKS target was not a pinned IPv4 address: %v", request)
			}
			if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
				return err
			}
			httpRequest, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				return err
			}
			defer httpRequest.Body.Close()
			if httpRequest.Host != "origin.example" {
				return fmt.Errorf("SOCKS request lost original Host: %s", httpRequest.Host)
			}
			_, err = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			return err
		}()
	}()
	proxyURL, _ := url.Parse("socks5h://proxy.example:1080")
	base := &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.4.4:1080" {
			return nil, fmt.Errorf("SOCKS proxy was not pinned: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	}}
	defer closePublicUserTransports(base)
	guard := &publicUserTransport{base: base, userProxy: true, lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
		ip := "8.8.8.8"
		if host == "proxy.example" {
			ip = "8.8.4.4"
		}
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}}
	resp, err := (&http.Client{Transport: guard, Timeout: 3 * time.Second}).Get("http://origin.example/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
