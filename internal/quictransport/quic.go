package quictransport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/transport"
	"github.com/mirmik/ariadne/internal/wire"
	quic "github.com/quic-go/quic-go"
)

const ALPN = "ariadne/2"

const closeCode quic.ApplicationErrorCode = 0x100

func Config() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout:  5 * time.Second,
		MaxIdleTimeout:        90 * time.Second,
		KeepAlivePeriod:       30 * time.Second,
		MaxIncomingStreams:    1,
		MaxIncomingUniStreams: -1,
		Allow0RTT:             false,
	}
}

func Dial(ctx context.Context, address string, tlsConfig *tls.Config) (messageconn.Conn, error) {
	if tlsConfig == nil {
		return nil, errors.New("QUIC TLS config is required")
	}
	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = []string{ALPN}
	connection, err := quic.DialAddr(ctx, address, tlsConfig, Config())
	if err != nil {
		return nil, fmt.Errorf("dial QUIC relay: %w", err)
	}
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		_ = connection.CloseWithError(closeCode, "open control stream failed")
		return nil, fmt.Errorf("open QUIC control stream: %w", err)
	}
	return framedConnection(connection, stream), nil
}

type Server struct {
	listener       *quic.Listener
	quicTransport  *quic.Transport
	packetConn     *net.UDPConn
	certificatePin string
	wg             sync.WaitGroup
	mu             sync.Mutex
	cancel         context.CancelFunc
	connections    map[*quic.Conn]struct{}
	closing        bool
}

func Listen(address, certificatePath, keyPath string) (*Server, error) {
	certificate, err := tls.LoadX509KeyPair(certificatePath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load QUIC TLS keypair: %w", err)
	}
	udpAddress, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve QUIC listen address: %w", err)
	}
	packetConnection, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for QUIC nodes: %w", err)
	}
	quicTransport := &quic.Transport{Conn: packetConnection}
	listener, err := quicTransport.Listen(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{ALPN},
		Certificates: []tls.Certificate{certificate},
	}, Config())
	if err != nil {
		_ = quicTransport.Close()
		return nil, fmt.Errorf("listen for QUIC nodes: %w", err)
	}
	return &Server{
		listener:       listener,
		quicTransport:  quicTransport,
		packetConn:     packetConnection,
		certificatePin: transport.FormatCertificatePin(certificate.Certificate[0]),
		connections:    make(map[*quic.Conn]struct{}),
	}, nil
}

func (server *Server) Addr() string {
	return server.listener.Addr().String()
}

func (server *Server) CertificatePin() string {
	return server.certificatePin
}

func (server *Server) Serve(ctx context.Context, handler func(messageconn.Conn)) error {
	serveContext, cancel := context.WithCancel(ctx)
	server.mu.Lock()
	server.cancel = cancel
	server.mu.Unlock()
	defer cancel()
	for {
		connection, err := server.listener.Accept(serveContext)
		if err != nil {
			if serveContext.Err() != nil {
				return serveContext.Err()
			}
			return err
		}
		server.mu.Lock()
		if server.closing {
			server.mu.Unlock()
			_ = connection.CloseWithError(closeCode, "server closed")
			return context.Canceled
		}
		server.connections[connection] = struct{}{}
		server.wg.Add(1)
		server.mu.Unlock()
		go func() {
			defer func() {
				server.mu.Lock()
				delete(server.connections, connection)
				server.mu.Unlock()
				server.wg.Done()
			}()
			streamContext, cancel := context.WithTimeout(serveContext, 10*time.Second)
			stream, err := connection.AcceptStream(streamContext)
			cancel()
			if err != nil {
				_ = connection.CloseWithError(closeCode, "control stream not opened")
				return
			}
			handler(framedConnection(connection, stream))
		}()
	}
}

func (server *Server) Close() error {
	server.mu.Lock()
	server.closing = true
	if server.cancel != nil {
		server.cancel()
	}
	connections := make([]*quic.Conn, 0, len(server.connections))
	for connection := range server.connections {
		connections = append(connections, connection)
	}
	server.mu.Unlock()
	err := server.listener.Close()
	for _, connection := range connections {
		_ = connection.CloseWithError(closeCode, "server closed")
	}
	server.wg.Wait()
	return errors.Join(err, server.quicTransport.Close(), server.packetConn.Close())
}

func framedConnection(connection *quic.Conn, stream *quic.Stream) messageconn.Conn {
	return messageconn.NewFramed(stream, uint32(wire.MaxControlMessageSize), func() {
		stream.CancelRead(quic.StreamErrorCode(closeCode))
		stream.CancelWrite(quic.StreamErrorCode(closeCode))
		_ = stream.Close()
		_ = connection.CloseWithError(closeCode, "connection closed")
	})
}
