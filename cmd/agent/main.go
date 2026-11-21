// Ligolo-ng
// Copyright (C) 2025 Nicolas Chatelain (nicocha30)

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/nicocha30/ligolo-ng/pkg/tlsutils"
	"github.com/nicocha30/ligolo-ng/pkg/utils"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/nicocha30/ligolo-ng/pkg/agent"
	"github.com/sirupsen/logrus"
	goproxy "golang.org/x/net/proxy"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		logrus.Fatal(err)
	}
}

// run parses args and runs the agent connection loop. Errors are returned
// rather than logged as fatal, so the same code can back the Sliver extension
// entrypoint without calling os.Exit() on the implant process.
func run(args []string) error {
	return runContext(context.Background(), args)
}

// runContext is run with a context, so a Sliver task can be stopped.
func runContext(ctx context.Context, args []string) error {
	var tlsConfig tls.Config
	fs := flag.NewFlagSet("ligolo-agent", flag.ContinueOnError)
	var ignoreCertificate = fs.Bool("ignore-cert", false, "ignore TLS certificate validation (dangerous), only for debug purposes")
	var acceptFingerprint = fs.String("accept-fingerprint", "", "accept certificates matching the following SHA256 fingerprint (hex format)")
	var verbose = fs.Bool("v", false, "enable verbose mode")
	var retry = fs.Bool("retry", false, "auto-retry on initial connection error")
	var retryDelay = fs.Int("retry-delay", 5, "retry delay in seconds for initial connection (default: 5)")
	var reconnect = fs.Bool("reconnect", true, "auto-reconnect after established connection is lost")
	var reconnectDelay = fs.Int("reconnect-delay", 20, "reconnection delay in seconds (default: 20)")
	var reconnectTimeout = fs.Int("reconnect-timeout", 300, "total reconnection timeout in seconds (default: 300 = 5 minutes)")
	var socksProxy = fs.String("proxy", "", "proxy URL address (http://admin:secret@127.0.0.1:8080) or socks://admin:secret@127.0.0.1:8080")
	var serverAddr = fs.String("connect", "", "connect to proxy (domain:port)")
	var bindAddr = fs.String("bind", "", "bind to ip:port")
	var userAgent = fs.String("ua", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/103.0.0.0 Safari/537.36", "HTTP User-Agent")
	var versionFlag = fs.Bool("version", false, "show the current version")

	fs.Usage = func() {
		fmt.Printf("Ligolo-ng %s / %s / %s\n", version, commit, date)
		fmt.Println("Made in France with love by @Nicocha30!")
		fmt.Println("https://github.com/nicocha30/ligolo-ng")
		fmt.Printf("\nUsage of %s:\n", os.Args[0])
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if *versionFlag {
		fmt.Printf("Ligolo-ng %s / %s / %s\n", version, commit, date)
		return nil
	}

	logrus.SetReportCaller(*verbose)

	if *verbose {
		logrus.SetLevel(logrus.DebugLevel)
	}

	if *bindAddr != "" {
		return bind(ctx, &tlsConfig, *bindAddr)
	}

	if *serverAddr == "" {
		return fmt.Errorf("please, specify the target host user -connect host:port")
	}

	ligoloUrl, err := utils.ParseLigoloURL(*serverAddr)
	if err != nil {
		return fmt.Errorf("invalid connect address, please use http(s)://host:port for websocket or host:port for tcp")
	}

	tlsConfig.ServerName = ligoloUrl.Hostname()

	if *ignoreCertificate {
		logrus.Warn("warning, certificate validation disabled")
		tlsConfig.InsecureSkipVerify = true
	}

	// Validate retry and reconnect delays
	if *retryDelay < 1 {
		return fmt.Errorf("retry-delay must be at least 1 second")
	}
	if *reconnectDelay < 1 {
		return fmt.Errorf("reconnect-delay must be at least 1 second")
	}
	if *reconnectTimeout < *reconnectDelay {
		return fmt.Errorf("reconnect-timeout must be greater than reconnect-delay")
	}

	var conn net.Conn
	connectionEstablished := false
	var reconnectStartTime time.Time
	var reconnectAttempt int

	for {
		var err error
		var connSuccess bool

		if ctx.Err() != nil {
			return nil
		}

		if ligoloUrl.IsWebsocket() {
			//websocket
			connSuccess, err = wsconnect(ctx, &tlsConfig, *serverAddr, *socksProxy, *userAgent)
		} else {
			if *socksProxy != "" {
				//suppose that scheme is socks:// or socks5://
				var proxyUrl *url.URL
				proxyUrl, err = url.Parse(*socksProxy)
				if err != nil {
					return fmt.Errorf("invalid proxy address, please use socks5://host:port")
				}
				if proxyUrl.Scheme == "http" {
					return fmt.Errorf("can't use http-proxy with direct (tcp) connection. Only with websocket")
				}
				if proxyUrl.Scheme == "socks" || proxyUrl.Scheme == "socks5" {
					pass, _ := proxyUrl.User.Password()
					conn, err = sockDial(*serverAddr, proxyUrl.Host, proxyUrl.User.Username(), pass)
				} else {
					return fmt.Errorf("invalid socks5 address, please use socks://host:port")
				}
			} else {
				conn, err = net.Dial("tcp", *serverAddr)
			}
			if err == nil {
				if *acceptFingerprint != "" {
					tlsConfig.InsecureSkipVerify = true
					tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
						crtFingerprint := sha256.Sum256(rawCerts[0])
						crtMatch, decodeErr := hex.DecodeString(*acceptFingerprint)
						if decodeErr != nil {
							return fmt.Errorf("invalid cert fingerprint: %v\n", decodeErr)
						}
						if bytes.Compare(crtMatch, crtFingerprint[:]) != 0 {
							return fmt.Errorf("certificate does not match fingerprint: %X != %X", crtFingerprint, crtMatch)
						}
						return nil
					}
				}
				tlsConn := tls.Client(conn, &tlsConfig)
				connSuccess, err = connect(ctx, tlsConn)
			}
		}

		// If connection was successful at any point, mark it as established
		if connSuccess {
			connectionEstablished = true
			// Reset reconnection tracking when successfully connected
			reconnectStartTime = time.Time{}
			reconnectAttempt = 0
		}

		// If we reach here, there was an error
		if err != nil {
			logrus.Errorf("Connection error: %v", err)
		}

		if ctx.Err() != nil {
			return nil
		}

		// Determine retry/reconnect behavior
		if !connectionEstablished {
			// Initial connection attempt failed
			if *retry {
				logrus.Infof("Retrying initial connection in %d seconds.", *retryDelay)
				if !sleepContext(ctx, time.Duration(*retryDelay)*time.Second) {
					return nil
				}
				continue
			} else {
				return fmt.Errorf("initial connection failed. Use -retry flag to enable automatic retry")
			}
		} else {
			// Connection was previously established but now lost
			if *reconnect {
				// Initialize reconnection tracking on first reconnect attempt
				if reconnectStartTime.IsZero() {
					logrus.Warn("Connection lost. Attempting to reconnect...")
					reconnectStartTime = time.Now()
					reconnectAttempt = 0
				}

				reconnectAttempt++
				elapsed := time.Since(reconnectStartTime)

				if elapsed.Seconds() >= float64(*reconnectTimeout) {
					return fmt.Errorf("reconnection timeout reached after %d attempts over %.0f seconds, giving up", reconnectAttempt, elapsed.Seconds())
				}

				logrus.Infof("Reconnection attempt %d (elapsed: %.0fs/%.0fs). Waiting %d seconds...", reconnectAttempt, elapsed.Seconds(), float64(*reconnectTimeout), *reconnectDelay)
				if !sleepContext(ctx, time.Duration(*reconnectDelay)*time.Second) {
					return nil
				}
			} else {
				return fmt.Errorf("connection lost and reconnect is disabled")
			}
		}
	}
}

// sleepContext sleeps for d, returning false if ctx is cancelled first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// sockDial is used when connecting using direct connection through a socks5 proxy server.
func sockDial(serverAddr string, socksProxy string, socksUser string, socksPass string) (net.Conn, error) {
	proxyDialer, err := goproxy.SOCKS5("tcp", socksProxy, &goproxy.Auth{
		User:     socksUser,
		Password: socksPass,
	}, goproxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("socks5 error: %w", err)
	}
	return proxyDialer.Dial("tcp", serverAddr)
}

// connect is used when connecting using direct connection.
func connect(ctx context.Context, conn net.Conn) (bool, error) {
	// Configure yamux with longer timeouts for multi-hop scenarios
	yamuxConfig := yamux.DefaultConfig()
	yamuxConfig.EnableKeepAlive = true
	yamuxConfig.KeepAliveInterval = 60 * time.Second       // Increased from 30s
	yamuxConfig.ConnectionWriteTimeout = 120 * time.Second // Increased from 60s
	yamuxConfig.MaxStreamWindowSize = 16 * 1024 * 1024     // 16MB window

	yamuxConn, err := yamux.Server(conn, yamuxConfig)
	if err != nil {
		return false, err
	}

	logrus.WithFields(logrus.Fields{"addr": conn.RemoteAddr()}).Info("Connection established")

	// Closing the connection unblocks Accept, so a cancelled task stops promptly.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			yamuxConn.Close()
			conn.Close()
		case <-done:
		}
	}()

	for {
		conn, err := yamuxConn.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return true, nil
			}
			return true, err // Connection was established but now lost
		}
		go agent.HandleConn(conn)
	}
}

func bind(ctx context.Context, config *tls.Config, bindAddr string) error {
	selfcrt := tlsutils.NewSelfCert(nil)
	crt, err := selfcrt.GetCertificate(bindAddr)
	if err != nil {
		return err
	}
	logrus.Warnf("TLS Certificate fingerprint is: %X\n", sha256.Sum256(crt.Certificate[0]))
	config.GetCertificate = func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return crt, nil
	}
	lis, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return err
	}
	defer lis.Close()
	go func() {
		<-ctx.Done()
		lis.Close()
	}()
	logrus.Infof("Listening on %s...", bindAddr)
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logrus.Error(err)
			continue
		}
		logrus.Infof("Got connection from: %s\n", conn.RemoteAddr())
		tlsConn := tls.Server(conn, config)

		if connSuccess, err := connect(ctx, tlsConn); err != nil {
			logrus.Error(err)
		} else if connSuccess {
			// This shouldn't happen as connect only returns on error, but for safety
			logrus.Info("Connection closed normally")
		}
	}
}

// wsconnect is used to connect using websockets.
func wsconnect(parent context.Context, config *tls.Config, wsaddr string, proxy string, useragent string) (bool, error) {
	//timeout for websocket library connection - 20 seconds
	ctx, cancel := context.WithTimeout(parent, time.Second*20)
	defer cancel()

	//in case of websocket proxy can be http with login:pass
	//Ex: proxystr = "http://admin:secret@127.0.0.1:8080"
	var proxyUrl *url.URL
	if proxy != "" {
		var err error
		proxyUrl, err = url.Parse(proxy)
		if err != nil {
			return false, fmt.Errorf("wsconnect: proxy url is invalid: %v", err)
		}
	}

	wsUrl, err := utils.ParseLigoloURL(wsaddr)
	if err != nil {
		return false, fmt.Errorf("wsconnect: wsaddr is invalid: %v", err)
	}

	httpTransport := &http.Transport{
		MaxIdleConns: http.DefaultMaxIdleConnsPerHost,
		Proxy:        http.ProxyURL(proxyUrl),
	}

	if wsUrl.IsSecure() {
		config.MinVersion = tls.VersionTLS11
		httpTransport.TLSClientConfig = config
	}

	httpClient := &http.Client{Transport: httpTransport}
	httpheader := &http.Header{}
	httpheader.Add("User-Agent", useragent)

	wsConn, _, err := websocket.Dial(ctx, wsaddr, &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: *httpheader})
	if err != nil {
		return false, err
	}

	netctx, cancelNet := context.WithCancel(parent)
	netConn := websocket.NetConn(netctx, wsConn, websocket.MessageBinary)
	defer cancelNet()

	// FIXED: Configure yamux with better timeout settings for websocket double-hop too
	yamuxConfig := yamux.DefaultConfig()
	yamuxConfig.EnableKeepAlive = true
	yamuxConfig.KeepAliveInterval = 60 * time.Second       // Increased
	yamuxConfig.ConnectionWriteTimeout = 120 * time.Second // Increased
	yamuxConfig.MaxStreamWindowSize = 16 * 1024 * 1024     // 16MB window

	yamuxConn, err := yamux.Server(netConn, yamuxConfig)
	if err != nil {
		return false, err
	}

	logrus.Info("Websocket connection established")
	// Closing the connection unblocks Accept, so a cancelled task stops promptly.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-parent.Done():
			yamuxConn.Close()
			netConn.Close()
		case <-done:
		}
	}()
	for {
		conn, err := yamuxConn.Accept()
		if err != nil {
			if parent.Err() != nil {
				return true, nil
			}
			return true, err // Connection was established but now lost
		}
		go agent.HandleConn(conn)
	}
}
