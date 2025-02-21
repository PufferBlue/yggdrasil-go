package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/Arceliar/phony"
	"github.com/pires/go-proxyproto"
)

type linkTLS struct {
	phony.Inbox
	*links
	tcp        *linkTCP
	listener   *net.ListenConfig
	config     *tls.Config
	_listeners map[*Listener]context.CancelFunc
}

func (l *links) newLinkTLS(tcp *linkTCP) *linkTLS {
	lt := &linkTLS{
		links: l,
		tcp:   tcp,
		listener: &net.ListenConfig{
			Control:   tcp.tcpContext,
			KeepAlive: -1,
		},
		_listeners: map[*Listener]context.CancelFunc{},
	}
	var err error
	lt.config, err = lt.generateConfig()
	if err != nil {
		panic(err)
	}
	return lt
}

func (l *linkTLS) dial(url *url.URL, options linkOptions, sintf, sni string) error {
	dialers, err := l.tcp.dialersFor(url, options, sintf)
	if err != nil {
		return err
	}
	if len(dialers) == 0 {
		return nil
	}
	for _, d := range dialers {
		tlsconfig := l.config.Clone()
		tlsconfig.ServerName = sni
		tlsdialer := &tls.Dialer{
			NetDialer: d.dialer,
			Config:    tlsconfig,
		}
		var conn net.Conn
		conn, err = tlsdialer.DialContext(l.core.ctx, "tcp", d.addr.String())
		if err != nil {
			continue
		}
		name := strings.TrimRight(strings.SplitN(url.String(), "?", 2)[0], "/")
		dial := &linkDial{
			url:   url,
			sintf: sintf,
		}
		return l.handler(dial, name, d.info, conn, options, false, false)
	}
	return fmt.Errorf("failed to connect via %d address(es), last error: %w", len(dialers), err)
}

func (l *linkTLS) listen(url *url.URL, sintf string) (*Listener, error) {
	ctx, cancel := context.WithCancel(l.core.ctx)
	hostport := url.Host
	if sintf != "" {
		if host, port, err := net.SplitHostPort(hostport); err == nil {
			hostport = fmt.Sprintf("[%s%%%s]:%s", host, sintf, port)
		}
	}
	listener, err := l.listener.Listen(ctx, "tcp", hostport)
	if err != nil {
		cancel()
		return nil, err
	}
	var tlslistener net.Listener
	var proxylistener proxyproto.Listener
	linkoptions := linkOptionsForListener(url)
	if linkoptions.proxyprotocol {
		proxylistener = proxyproto.Listener{Listener: listener}
		tlslistener = tls.NewListener(&proxylistener, l.config)
		l.core.log.Printf("ProxyProtocol enabled for TLS listener %s", listener.Addr())
	} else {
		tlslistener = tls.NewListener(listener, l.config)
	}
	entry := &Listener{
		Listener: tlslistener,
		closed:   make(chan struct{}),
	}
	phony.Block(l, func() {
		l._listeners[entry] = cancel
	})
	l.core.log.Printf("TLS listener started on %s", listener.Addr())
	go func() {
		defer phony.Block(l, func() {
			delete(l._listeners, entry)
		})
		for {
			conn, err := tlslistener.Accept()
			if err != nil {
				cancel()
				break
			}
			laddr := conn.LocalAddr().(*net.TCPAddr)
			raddr := conn.RemoteAddr().(*net.TCPAddr)
			name := fmt.Sprintf("tls://%s", raddr)
			info := linkInfoFor("tls", sintf, tcpIDFor(laddr, raddr))
			if err = l.handler(nil, name, info, conn, linkOptionsForListener(url), true, raddr.IP.IsLinkLocalUnicast()); err != nil {
				l.core.log.Errorln("Failed to create inbound link:", err)
			}
		}
		_ = tlslistener.Close()
		close(entry.closed)
		l.core.log.Printf("TLS listener stopped on %s", listener.Addr())
	}()
	return entry, nil
}

// RFC5280 section 4.1.2.5
var notAfterNeverExpires = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

func (l *linkTLS) generateConfig() (*tls.Config, error) {
	certBuf := &bytes.Buffer{}
	cert := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: hex.EncodeToString(l.links.core.public[:]),
		},
		NotBefore:             time.Now(),
		NotAfter:              notAfterNeverExpires,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certbytes, err := x509.CreateCertificate(rand.Reader, &cert, &cert, l.links.core.public, l.links.core.secret)
	if err != nil {
		return nil, err
	}

	if err := pem.Encode(certBuf, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certbytes,
	}); err != nil {
		return nil, err
	}

	rootCAs := x509.NewCertPool()
	rootCAs.AppendCertsFromPEM(certbytes)

	return &tls.Config{
		RootCAs: rootCAs,
		Certificates: []tls.Certificate{
			{
				Certificate: [][]byte{certbytes},
				PrivateKey:  l.links.core.secret,
			},
		},
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	}, nil
}

func (l *linkTLS) handler(dial *linkDial, name string, info linkInfo, conn net.Conn, options linkOptions, incoming, force bool) error {
	return l.tcp.handler(dial, name, info, conn, options, incoming, force)
}
