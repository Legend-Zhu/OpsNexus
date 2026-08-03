package docker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// buildTransport configures an *http.Transport and base URL for the given
// DOCKER_HOST value. Supported schemes:
//
//   - unix:///path/to/docker.sock   (default; Linux production)
//   - tcp://host:2375               (plain; development only)
//   - tcp://host:2376               (TLS; production over network)
//   - http://host:2375, https://host:2376
//
// Windows named-pipe (npipe://) is intentionally not supported here to keep
// the dependency surface stdlib-only; on Windows use Docker Desktop's
// "Expose daemon on tcp://localhost:2375" and set DOCKER_HOST accordingly.
func buildTransport(host string, tlsCfg *tls.Config) (baseURL string, tr *http.Transport, err error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}

	switch {
	case strings.HasPrefix(host, "unix://"):
		sockPath := strings.TrimPrefix(host, "unix://")
		tr = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "unix", sockPath)
			},
		}
		baseURL = "http://docker"

	case strings.HasPrefix(host, "tcp://"):
		rest := strings.TrimPrefix(host, "tcp://")
		if tlsCfg != nil {
			baseURL = "https://" + rest
		} else {
			baseURL = "http://" + rest
		}
		tr = &http.Transport{TLSClientConfig: tlsCfg}

	case strings.HasPrefix(host, "https://"), strings.HasPrefix(host, "http://"):
		baseURL = host
		tr = &http.Transport{TLSClientConfig: tlsCfg}

	default:
		return "", nil, fmt.Errorf("unsupported DOCKER_HOST scheme: %q", host)
	}
	return baseURL, tr, nil
}

// loadTLSConfig builds a TLS config from the certificate directory
// (DOCKER_CERT_PATH, default ~/.docker). Returns nil (no TLS) when
// DOCKER_TLS_VERIFY is unset/empty.
func loadTLSConfig() (*tls.Config, error) {
	if os.Getenv("DOCKER_TLS_VERIFY") == "" {
		return nil, nil
	}
	dir := os.Getenv("DOCKER_CERT_PATH")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir for docker certs: %w", err)
		}
		dir = home + "/.docker"
	}

	caPEM, err := os.ReadFile(dir + "/ca.pem")
	if err != nil {
		return nil, fmt.Errorf("read ca.pem: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certs in ca.pem")
	}
	cert, err := tls.LoadX509KeyPair(dir+"/cert.pem", dir+"/key.pem")
	if err != nil {
		return nil, fmt.Errorf("load client cert/key: %w", err)
	}
	return &tls.Config{
		RootCAs:            caPool,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS12,
	}, nil
}
