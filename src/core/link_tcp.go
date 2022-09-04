package core

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/Arceliar/phony"
	"golang.org/x/net/proxy"
)

type linkTCP struct {
	phony.Inbox
	*links
	listener   *net.ListenConfig
	_listeners map[net.Listener]context.CancelFunc
}

type tcpOptions struct {
	linkOptions
	socksProxyAddr string      // nolint:unused
	socksProxyAuth *proxy.Auth // nolint:unused
	socksPeerAddr  string      // nolint:unused
	tlsSNI         string
}

func (l *links) newLinkTCP() *linkTCP {
	lt := &linkTCP{
		links: l,
		listener: &net.ListenConfig{
			KeepAlive: -1,
		},
		_listeners: map[net.Listener]context.CancelFunc{},
	}
	lt.listener.Control = lt.tcpContext
	return lt
}

func (l *linkTCP) dial(url *url.URL, options tcpOptions, sintf string) error {
	info := linkInfoFor("tcp", url.Host, sintf)
	if l.links.isConnectedTo(info) {
		return fmt.Errorf("duplicate connection attempt")
	}
	addr, err := net.ResolveTCPAddr("tcp", url.Host)
	if err != nil {
		return err
	}
	addr.Zone = sintf
	dialer, err := l.dialerFor(addr.String(), sintf)
	if err != nil {
		return err
	}
	conn, err := dialer.DialContext(l.core.ctx, "tcp", addr.String())
	if err != nil {
		return err
	}
	return l.handler(url.String(), info, conn, options, false)
}

func (l *linkTCP) listen(url *url.URL, sintf string) (*Listener, error) {
	ctx, cancel := context.WithCancel(l.core.ctx)
	hostport := url.Host
	if sintf != "" {
		host, port, err := net.SplitHostPort(hostport)
		if err == nil {
			hostport = fmt.Sprintf("[%s%%%s]:%s", host, sintf, port)
		}
	}
	listener, err := l.listener.Listen(ctx, "tcp", hostport)
	if err != nil {
		cancel()
		return nil, err
	}
	phony.Block(l, func() {
		l._listeners[listener] = cancel
	})
	go func() {
		defer phony.Block(l, func() {
			delete(l._listeners, listener)
		})
		for {
			conn, err := listener.Accept()
			if err != nil {
				cancel()
				return
			}
			name := fmt.Sprintf("tcp://%s", conn.RemoteAddr())
			info := linkInfoFor("tcp", sintf, conn.RemoteAddr().String())
			if err = l.handler(name, info, conn, tcpOptions{}, true); err != nil {
				l.core.log.Errorln("Failed to create inbound link:", err)
			}
		}
	}()
	return &Listener{
		Listener: listener,
		Close:    cancel,
	}, nil
}

func (l *linkTCP) handler(name string, info linkInfo, conn net.Conn, options tcpOptions, incoming bool) error {
	return l.links.create(
		conn,                // connection
		name,                // connection name
		info,                // connection info
		incoming,            // not incoming
		false,               // not forced
		options.linkOptions, // connection options
	)
}

// Returns the address of the listener.
func (l *linkTCP) getAddr() *net.TCPAddr {
	// TODO: Fix this, because this will currently only give a single address
	// to multicast.go, which obviously is not great, but right now multicast.go
	// doesn't have the ability to send more than one address in a packet either
	var addr *net.TCPAddr
	phony.Block(l, func() {
		for listener := range l._listeners {
			addr = listener.Addr().(*net.TCPAddr)
		}
	})
	return addr
}

func (l *linkTCP) dialerFor(saddr, sintf string) (*net.Dialer, error) {
	dst, err := net.ResolveTCPAddr("tcp", saddr)
	if err != nil {
		return nil, err
	}
	if dst.IP.IsLinkLocalUnicast() {
		dst.Zone = sintf
		if dst.Zone == "" {
			return nil, fmt.Errorf("link-local address requires a zone")
		}
	}
	dialer := &net.Dialer{
		Timeout:   time.Second * 5,
		KeepAlive: -1,
		Control:   l.tcpContext,
	}
	if sintf != "" {
		dialer.Control = l.getControl(sintf)
		ief, err := net.InterfaceByName(sintf)
		if err != nil {
			return nil, fmt.Errorf("interface %q not found", sintf)
		}
		if ief.Flags&net.FlagUp == 0 {
			return nil, fmt.Errorf("interface %q is not up", sintf)
		}
		addrs, err := ief.Addrs()
		if err != nil {
			return nil, fmt.Errorf("interface %q addresses not available: %w", sintf, err)
		}
		for addrindex, addr := range addrs {
			src, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			if src.Equal(dst.IP) {
				continue
			}
			if !src.IsGlobalUnicast() && !src.IsLinkLocalUnicast() {
				continue
			}
			bothglobal := src.IsGlobalUnicast() == dst.IP.IsGlobalUnicast()
			bothlinklocal := src.IsLinkLocalUnicast() == dst.IP.IsLinkLocalUnicast()
			if !bothglobal && !bothlinklocal {
				continue
			}
			if (src.To4() != nil) != (dst.IP.To4() != nil) {
				continue
			}
			if bothglobal || bothlinklocal || addrindex == len(addrs)-1 {
				dialer.LocalAddr = &net.TCPAddr{
					IP:   src,
					Port: 0,
					Zone: sintf,
				}
				break
			}
		}
		if dialer.LocalAddr == nil {
			return nil, fmt.Errorf("no suitable source address found on interface %q", sintf)
		}
	}
	return dialer, nil
}
