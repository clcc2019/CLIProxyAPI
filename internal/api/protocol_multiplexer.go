package api

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const muxProtocolDetectionTimeout = 5 * time.Second

func normalizeHTTPServeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func normalizeListenerError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) acceptMuxConnections(listener net.Listener, httpListener *muxListener) error {
	if s == nil || listener == nil {
		return net.ErrClosed
	}

	for {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			return errAccept
		}
		if conn == nil {
			continue
		}
		go s.routeMuxConnection(conn, httpListener)
	}
}

func (s *Server) routeMuxConnection(conn net.Conn, httpListener *muxListener) {
	if conn == nil {
		return
	}

	if muxProtocolDetectionTimeout > 0 {
		if err := conn.SetDeadline(time.Now().Add(muxProtocolDetectionTimeout)); err != nil {
			log.Debugf("failed to set protocol detection deadline: %v", err)
		}
	}
	clearDeadline := func() {
		if muxProtocolDetectionTimeout <= 0 {
			return
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			log.Debugf("failed to clear protocol detection deadline: %v", err)
		}
	}

	tlsConn, ok := conn.(*tls.Conn)
	if ok {
		if errHandshake := tlsConn.Handshake(); errHandshake != nil {
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("failed to close connection after TLS handshake error: %v", errClose)
			}
			return
		}
		proto := strings.TrimSpace(tlsConn.ConnectionState().NegotiatedProtocol)
		if proto == "h2" || proto == "http/1.1" {
			clearDeadline()
			if httpListener == nil {
				if errClose := conn.Close(); errClose != nil {
					log.Errorf("failed to close connection: %v", errClose)
				}
				return
			}
			if errPut := httpListener.Put(tlsConn); errPut != nil {
				if errClose := conn.Close(); errClose != nil {
					log.Errorf("failed to close connection after HTTP routing failure: %v", errClose)
				}
			}
			return
		}
	}

	reader := bufio.NewReader(conn)
	prefix, errPeek := reader.Peek(1)
	if errPeek != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("failed to close connection after protocol peek failure: %v", errClose)
		}
		return
	}

	if isRedisRESPPrefix(prefix[0]) {
		clearDeadline()
		if !s.managementRoutesEnabled.Load() {
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("failed to close redis connection while management is disabled: %v", errClose)
			}
			return
		}
		go s.handleRedisConnection(conn, reader)
		return
	}

	if httpListener == nil {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("failed to close connection without HTTP listener: %v", errClose)
		}
		return
	}

	clearDeadline()
	if errPut := httpListener.Put(&bufferedConn{Conn: conn, reader: reader}); errPut != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("failed to close connection after HTTP routing failure: %v", errClose)
		}
	}
}
