package router

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/gammazero/nexus/stdlog"
	"github.com/gammazero/nexus/transport"
	"github.com/gammazero/nexus/transport/serialize"
	"github.com/gammazero/nexus/wamp"
	"github.com/gorilla/websocket"
)

const (
	jsonWebsocketProtocol    = "wamp.2.json"
	msgpackWebsocketProtocol = "wamp.2.msgpack"
)

type protocol struct {
	payloadType int
	serializer  serialize.Serializer
}

// WebsocketServer handles websocket connections.
type WebsocketServer struct {
	Upgrader *websocket.Upgrader

	// Serializer for text frames.  Defaults to JSONSerializer.
	TextSerializer serialize.Serializer
	// Serializer for binary frames.  Defaults to MessagePackSerializer.
	BinarySerializer serialize.Serializer

	router Router

	protocols map[string]protocol
	log       stdlog.StdLog

	enableTrackingCookie bool
	enableRequestCapture bool
}

// NewWebsocketServer takes a router instance and creates a new websocket
// server.  To run the websocket server, call one of the server's
// ListenAndServe methods:
//
//     s := NewWebsocketServer(r)
//     closer, err := s.ListenAndServe(address)
//
// Or, use the various ListenAndServe functions provided by net/http.  This
// works because WebsocketServer implements the http.Handler interface:
//
//     s := NewWebsocketServer(r)
//     server := &http.Server{
//         Handler: s,
//         Addr:    address,
//     }
//     server.ListenAndServe()
func NewWebsocketServer(r Router) *WebsocketServer {
	s := &WebsocketServer{
		router:    r,
		protocols: map[string]protocol{},
		log:       r.Logger(),
	}
	s.Upgrader = &websocket.Upgrader{}
	s.addProtocol(jsonWebsocketProtocol, websocket.TextMessage,
		&serialize.JSONSerializer{})
	s.addProtocol(msgpackWebsocketProtocol, websocket.BinaryMessage,
		&serialize.MessagePackSerializer{})

	return s
}

// SetConfig applies optional configuration settings to the websocket server.
func (s *WebsocketServer) SetConfig(wsCfg transport.WebsocketConfig) {
	if wsCfg.EnableCompression {
		s.Upgrader.EnableCompression = true
		// Uncomment after https://github.com/gorilla/websocket/pull/342
		//s.Upgrader.CompressionLevel = wsCfg.CompressionLevel
		//s.Upgrader.EnableContextTakeover = wsCfg.EnableContextTakeover
	}
	s.enableTrackingCookie = wsCfg.EnableTrackingCookie
	s.enableRequestCapture = wsCfg.EnableRequestCapture
}

// ListenAndServe listens on the specified TCP address and starts a goroutine
// that accepts new client connections until the returned io.closer is closed.
func (s *WebsocketServer) ListenAndServe(address string) (io.Closer, error) {
	l, err := net.Listen("tcp", address)
	if err != nil {
		s.log.Print(err)
		return nil, err
	}

	// Run service on configured port.
	server := &http.Server{
		Handler: s,
		Addr:    l.Addr().String(),
	}
	go server.Serve(l)
	return l, nil
}

// ListenAndServeTLS listens on the specified TCP address and starts a
// goroutine that accepts new TLS client connections until the returned
// io.closer is closed.  If tls.Config does not already contain a certificate,
// then certFile and keyFile, if specified, are used to load an X509
// certificate.
func (s *WebsocketServer) ListenAndServeTLS(address string, tlscfg *tls.Config, certFile, keyFile string) (io.Closer, error) {
	// With Go 1.9, code below, until tls.Listen, can be removed when using:
	//go server.ServeTLS(l, certFile, keyFile)
	var hasCert bool
	if tlscfg == nil {
		tlscfg = &tls.Config{}
	} else if len(tlscfg.Certificates) > 0 || tlscfg.GetCertificate != nil {
		hasCert = true
	}

	if !hasCert || certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("error loading X509 key pair: %s", err)
		}
		tlscfg.Certificates = append(tlscfg.Certificates, cert)
	}

	l, err := tls.Listen("tcp", address, tlscfg)
	if err != nil {
		s.log.Print(err)
		return nil, err
	}

	// Run service on configured port.
	server := &http.Server{
		Handler:   s,
		Addr:      l.Addr().String(),
		TLSConfig: tlscfg,
	}

	go server.Serve(l)
	//go server.ServeTLS(l, certFile, keyFile)

	return l, nil
}

// ServeHTTP handles HTTP connections.
func (s *WebsocketServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var authDict wamp.Dict
	var nextCookie *http.Cookie

	// If tracking cookie is enabled, then read the tracking cookie from the
	// request header, if it contains the cookie.  Generate a new tracking
	// cookie for next time and put it in the response header.
	if s.enableTrackingCookie {
		if authDict == nil {
			authDict = wamp.Dict{}
		}
		const cookieName = "nexus-wamp-cookie"
		if reqCk, err := r.Cookie(cookieName); err == nil {
			authDict["cookie"] = reqCk
			//fmt.Println("===> Received tracking cookie: ", reqCk)
		}
		b := make([]byte, 18)
		_, err := rand.Read(b)
		if err == nil {
			// Create new auth cookie with 20 byte random value.
			nextCookie = &http.Cookie{
				Name:  cookieName,
				Value: base64.URLEncoding.EncodeToString(b),
			}
			http.SetCookie(w, nextCookie)
			authDict["nextcookie"] = nextCookie
			//fmt.Println("===> Sent Next tracking cookie:", nextCookie)
			//fmt.Println()
		}
	}

	// If request capture is enabled, then the HTTP upgrade http.Request in the
	// HELLO and session details as transport.auth details.request.
	if s.enableRequestCapture {
		if authDict == nil {
			authDict = wamp.Dict{}
		}
		authDict["request"] = r
	}

	conn, err := s.Upgrader.Upgrade(w, r, w.Header())
	if err != nil {
		s.log.Println("Error upgrading to websocket connection:", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleWebsocket(conn, wamp.Dict{"auth": authDict})
}

// addProtocol registers a serializer for protocol and payload type.
func (s *WebsocketServer) addProtocol(proto string, payloadType int, serializer serialize.Serializer) error {
	if payloadType != websocket.TextMessage && payloadType != websocket.BinaryMessage {
		return fmt.Errorf("invalid payload type: %d", payloadType)
	}
	if _, ok := s.protocols[proto]; ok {
		return errors.New("protocol already registered: " + proto)
	}
	s.protocols[proto] = protocol{payloadType, serializer}
	s.Upgrader.Subprotocols = append(s.Upgrader.Subprotocols, proto)
	return nil
}

func (s *WebsocketServer) handleWebsocket(conn *websocket.Conn, transportDetails wamp.Dict) {
	var serializer serialize.Serializer
	var payloadType int
	// Get serializer and payload type for protocol.
	if proto, ok := s.protocols[conn.Subprotocol()]; ok {
		serializer = proto.serializer
		payloadType = proto.payloadType
	} else {
		// Although gorilla rejects connections with unregistered protocols,
		// other websocket implementations may not.
		switch conn.Subprotocol() {
		case jsonWebsocketProtocol:
			serializer = &serialize.JSONSerializer{}
			payloadType = websocket.TextMessage
		case msgpackWebsocketProtocol:
			serializer = &serialize.MessagePackSerializer{}
			payloadType = websocket.BinaryMessage
		default:
			conn.Close()
			return
		}
	}

	// Create a websocket peer from the websocket connection and attach the
	// peer to the router.
	peer := transport.NewWebsocketPeer(conn, serializer, payloadType, s.log)
	if err := s.router.AttachClient(peer, transportDetails); err != nil {
		s.log.Println("Error attaching to router:", err)
	}
}
