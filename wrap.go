package curvetls

import (
	"crypto/rand"
	"fmt"
	"net"
	"time"
)

// NewLongNonce generates a long nonce for use with curvetls.WrapServer
// and curvetls.WrapClient.
// A long nonce is needed and must be unique per long-term private key,
// whether the private key belongs to the server or the client.
// Long nonces must not be reused for new private keys.
func NewLongNonce() (*longNonce, error) {
	var nonce longNonce
	n, err := rand.Reader.Read(nonce[:])
	if err != nil {
		return nil, fmt.Errorf("error reading entropy while generating long nonce: %s", err)
	}
	if n != len(nonce) {
		return nil, fmt.Errorf("short entropy read while generating long nonce")
	}
	return &nonce, nil
}

// EncryptedConn is the opaque structure representing an encrypted connection.
///
// Use WrapClient() or WrapServer() to obtain one, and use its methods to
// engage in secure communication.
//
// Lifecycle Information
//
// In general, it is not thread safe to perform reads or writes on an
// EncryptedConn while any part of a handshake (WrapClient(), WrapServer(),
// Allow() or Deny()) is going on in a different goroutine.  You should
// complete the handshakes on a single goroutine.  It is also not safe to
// perform reads simultaneously on two or more goroutines.  It is also not
// safe to perform writes simultaneously on two or more goroutines.  It is
// also not safe to intersperse calls to Read() and ReadFrame(), even from
// the same goroutine.
//
// Concurrent things that are safe: (1) one read and one write each on a
// distinct goroutine (2) same as (1) while Close() is invoked on another
// goroutine (the ongoing read and write should return normally with an EOF
// or UnexpectedEOF in that case). (3) performing any operation on one
// EncryptedConn in a single goroutine, while any other operation on
// another EncryptedConn is ongoing in another goroutine.  There is no
// global mutable state shared among EncryptedConn instances.
type EncryptedConn struct {
	conn           net.Conn
	myNonce        *shortNonce
	theirNonce     *shortNonce
	myPrivkey      Privkey
	theirPubkey    Pubkey
	isServer       bool
	recvFrame      []byte
	recvMessageCmd *messageCommand
	sendMessageCmd *messageCommand
}

// Close closes the connection, including the underlying socket.
//
// It is an error to use Close() twice or to use other methods after Close().
func (w *EncryptedConn) Close() error {
	return w.conn.Close()
}

// LocalAddr() retrieves the local peer net.Addr of the underlying socket.
func (w *EncryptedConn) LocalAddr() net.Addr {
	return w.conn.LocalAddr()
}

// RemoteAddr() retrieves the remote peer net.Addr of the underlying socket.
func (w *EncryptedConn) RemoteAddr() net.Addr {
	return w.conn.RemoteAddr()
}

// Read reads one frame from the other side, decrypts the encrypted frame,
// then copies the bytes read to the passed slice.
//
// If the destination buffer is not large enough to contain the whole
// received frame, then a partial read is made and written to the buffer,
// and subsequent Read() calls will continue reading the remainder
// of the frame.
//
// If this function returns an error, the socket remains open, but
// (much like TLS) it is highly unlikely that, after returning an error,
// the connection will continue working.
//
// It is an error to invoke an EncryptedConn's Read() from a goroutine
// while another goroutine is invoking Read() or ReadFrame() on the same
// EncryptedConn.  Even with plain old sockets, you'd get nothing but
// corrupted reads that way.  It should, however, be safe to invoke Read()
// on an EncryptedConn within one goroutine while another goroutine invokes
// Write() on the same EncryptedConn.
func (w *EncryptedConn) Read(b []byte) (int, error) {
	if w.recvFrame == nil {
		frame, err := w.ReadFrame()
		if err != nil {
			return 0, nil
		}
		w.recvFrame = frame
	}
	n := copy(b, w.recvFrame)
	w.recvFrame = w.recvFrame[n:]
	if len(w.recvFrame) == 0 {
		w.recvFrame = nil
	}
	return n, nil
}

// ReadFrame reads one frame from the other side, decrypts the encrypted frame,
// then returns the whole frame as a slice of bytes.
//
// If this function returns an error, the socket remains open, but
// (much like TLS) it is highly unlikely that, after returning an error,
// the connection will continue working.
//
// It is an error to call ReadFrame when a previous Read was only partially
// written to its output buffer.
//
// It is an error to invoke an EncryptedConn's ReadFrame() from a goroutine
// while another goroutine is invoking ReadFrame() or Read() on the same
// EncryptedConn.  Even with plain old sockets, you'd get nothing but
// corruption that way.  It should, however, be safe to invoke ReadFrame()
// on an EncryptedConn within one goroutine while another goroutine invokes
// Write() on the same EncryptedConn.
func (w *EncryptedConn) ReadFrame() ([]byte, error) {
	if w.recvFrame != nil {
		return nil, newInternalError("cannot read a frame while there is a prior partial frame buffered")
	}
	/* Read and validate message. */

	// The following chunk altering w is safe so long as it is never
	// invoked simultaneously from two goroutines.
	//
	// Two things change within w (and linked members) when this code runs:
	//
	// 1. 8 bytes in w itself, when w gets written to, in order to
	//    store the buffer.  Changes to this part of w do not need
	//    to be visible in causal order to goroutines running
	//    Write()s in order for those Write()s to execute successfully.
	// 2. 0 bytes in w proper, but an uint64 value pointed to by
	//    the theirNonce member does get incremented.  Again, this does
	//    not affect w, or concurrent Write()s.

	if w.recvMessageCmd == nil {
		w.recvMessageCmd = &messageCommand{}
	}
	if err := readFrame(w.conn, w.recvMessageCmd); err != nil {
		return nil, err
	}

	data, err := w.recvMessageCmd.validate(w.theirNonce, w.myPrivkey, w.theirPubkey, w.isServer)
	if err != nil {
		if err == errNonceOverflow {
			return nil, newProtocolError("%s", err)
		}
		return nil, newInternalError("invalid MESSAGE: %s", err)
	}
	return data, nil
}

// Write frames, encrypts and sends to the other side the passed bytes.
//
// If this function returns an error, the socket remains open, but
// (much like TLS) it is highly unlikely that, after returning an error,
// the connection will continue working.
//
// It is an error to invoke Write() on the same EncryptedConn simultaneously
// from two goroutines.  Even with plain old sockets, you'd get nothing but
// corruption that way.  It should, however, be safe to invoke Write()
// on an EncryptedConn within one goroutine while another goroutine invokes
// Read() or ReadFrame() on the same EncryptedConn.
func (w *EncryptedConn) Write(b []byte) (int, error) {
	/* Build and send message. */

	// The following chunk altering w is safe so long as it is never
	// invoked simultaneously from two goroutines.
	//
	// Two things change within w (and linked members) when this code runs:
	//
	// 1. 8 bytes in w itself, when w gets written to, in order to
	//    store the buffer.  Changes to this part of w do not need
	//    to be visible in causal order to goroutines running
	//    ReadFrame()s in order for those ReadFrame()s to run correctly.
	// 2. 0 bytes in w proper, but an uint64 value pointed to by
	//    the myNonce member does get incremented.  Again, this does
	//    not affect w, or concurrent ReadFrame()s.
	if w.sendMessageCmd == nil {
		w.sendMessageCmd = &messageCommand{}
	}
	err := w.sendMessageCmd.build(w.myNonce, w.myPrivkey, w.theirPubkey, b, w.isServer)
	if err != nil {
		if err == errNonceOverflow {
			return 0, newProtocolError("%s", err)
		}
		return 0, newInternalError("cannot build MESSAGE: %s", err)
	}

	if err := writeFrame(w.conn, w.sendMessageCmd); err != nil {
		return 0, err
	}
	return len(b), nil
}

// SetDeadline calls SetDeadline on the underlying socket.
func (w *EncryptedConn) SetDeadline(t time.Time) error {
	return w.conn.SetDeadline(t)
}

// SetReadDeadline calls SetReadDeadline on the underlying socket.
func (w *EncryptedConn) SetReadDeadline(t time.Time) error {
	return w.conn.SetReadDeadline(t)
}

// SetWriteDeadline calls SetWriteDeadline on the underlying socket.
func (w *EncryptedConn) SetWriteDeadline(t time.Time) error {
	return w.conn.SetWriteDeadline(t)
}

// WrapServer wraps an existing, connected net.Conn with encryption and framing.
//
// Returned Values
//
// A net.Conn compatible object that you can use to send and receive data.
// Data sent and received will be framed and encrypted.  Once you have received
// this return value, you must call either Allow() or Deny() on the returned
// value in order to let the client know whether it is allowed to continue.
//
// The public key of the client; use this key to check that the
// client is authorized to continue the conversation, then either
// call Allow() to signal to the client that it is authorized, or
// call Deny() to signal to the client that it is not authorized
// and terminate the connection.
//
// An error.  It can be an underlying socket error, an internal error produced
// by a bug in the library, or a protocol error indicating that the
// communication encountered corrupt or malformed data from the peer.
// No method is provided to distinguish among these errors because
// the only sane thing to do at that point is to close the connection.
//
// Lifecycle Information
//
// If WrapServer() returns an error, the passed socket will have been closed
// by the time this function returns.
//
// Upon successful return of this function, the Close() method of the returned
// net.Conn will also Close() the passed net.Conn.
//
// If you read or write any data to the underlying socket rather
// than go through the returned socket, your data will be transmitted
// in plaintext and the endpoint will become confused and close the
// connection.  Don't do that.
func WrapServer(conn net.Conn,
	serverprivkey Privkey,
	serverpubkey Pubkey,
	long_nonce *longNonce) (*EncryptedConn, Pubkey, error) {

	bail := func(e error) (*EncryptedConn, Pubkey, error) {
		// These are unrecoverable errors.  We close the socket.
		conn.Close()
		return nil, Pubkey{}, e
	}

	myNonce := newShortNonce()
	clientNonce := newShortNonce()

	/* Do greeting. */
	var mygreeting, theirgreeting, expectedgreeting greeting
	mygreeting.asServer()
	expectedgreeting.asClient()

	if err := wrc(conn, mygreeting[:], theirgreeting[:]); err != nil {
		return bail(err)
	}

	if theirgreeting != expectedgreeting {
		return bail(newProtocolError("malformed greeting"))
	}

	/* Read and validate hello. */
	var helloCmd helloCommand
	if err := readFrame(conn, &helloCmd); err != nil {
		return bail(err)
	}

	ephClientPubkey, err := helloCmd.validate(clientNonce, permanentServerPrivkey(serverprivkey))
	if err != nil {
		return bail(newProtocolError("invalid HELLO: %s", err))
	}

	/* Build and send welcome. */
	var welcomeCmd welcomeCommand
	cookieKey, err := welcomeCmd.build(long_nonce, ephClientPubkey, permanentServerPrivkey(serverprivkey))
	// FIXME: wipe memory of cookiekey after 60 seconds
	// FIXME: wipe memory of cookie, and all the ephemeral server keys at this point
	if err != nil {
		if err == errNonceOverflow {
			return bail(newProtocolError("%s", err))
		}
		return bail(newInternalError("cannot build WELCOME: %s", err))
	}

	if err := writeFrame(conn, &welcomeCmd); err != nil {
		return bail(err)
	}

	/* Read and validate initiate. */
	var initiateCmd initiateCommand
	if err := readFrame(conn, &initiateCmd); err != nil {
		return bail(err)
	}

	permClientPubkey, ephClientPubkey, ephServerPrivkey, err := initiateCmd.validate(clientNonce, permanentServerPubkey(serverpubkey), cookieKey)
	if err != nil {
		return bail(newProtocolError("invalid INITIATE: %s", err))
	}

	return &EncryptedConn{
		conn:        conn,
		myNonce:     myNonce,
		theirNonce:  clientNonce,
		myPrivkey:   Privkey(ephServerPrivkey),
		theirPubkey: Pubkey(ephClientPubkey),
		isServer:    true,
	}, Pubkey(permClientPubkey), nil
}

// WrapClient wraps an existing, connected net.Conn with encryption and framing.
//
// Returned Values
//
// A net.Conn compatible object that you can use to send and receive data.
// Data sent and received will be framed and encrypted.
//
// An error.  It can be an underlying socket error, an internal error produced
// by a bug in the library, a protocol error indicating that the
// communication encountered corrupt or malformed data from the peer, or an
// authentication error.  A method to distinguish authentication errors
// is provided by the IsAuthenticationError() function.  No method is provided
// to distinguish among the other errors because the only sane thing to do at
// that point is to close the connection.
//
// Lifecycle Information
//
// If WrapClient() returns an error, the passed socket will have been closed
// by the time this function returns.
//
// Upon successful return of this function, the Close() method of the returned
// net.Conn will also Close() the passed net.Conn.
//
// Upon unauthorized use (the server rejects the client with Deny())
// this function will return an error which can be checked with
// the function IsAuthenticationError().  See note on Deny()
// to learn more about reconnection policy.
//
// If you read or write any data to the underlying socket rather
// than go through the returned socket, your data will be transmitted
// in plaintext and the endpoint will become confused and close the
// connection.  Don't do that.
func WrapClient(conn net.Conn,
	clientprivkey Privkey, clientpubkey Pubkey,
	permServerPubkey Pubkey,
	long_nonce *longNonce) (*EncryptedConn, error) {

	bail := func(e error) (*EncryptedConn, error) {
		// These are unrecoverable errors.  We close the socket.
		conn.Close()
		return nil, e
	}

	myNonce := newShortNonce()
	serverNonce := newShortNonce()

	/* Generate ephemeral keypair for this connection. */
	ephClientPrivkey, ephClientPubkey, err := genEphemeralClientKeyPair()
	if err != nil {
		return bail(newInternalError("cannot generate ephemeral keypair", err))
	}

	/* Do greeting. */
	var mygreeting, theirgreeting, expectedgreeting greeting
	mygreeting.asClient()
	expectedgreeting.asServer()

	if err := wrc(conn, mygreeting[:], theirgreeting[:]); err != nil {
		return bail(err)
	}

	if theirgreeting != expectedgreeting {
		return bail(newProtocolError("malformed greeting"))
	}

	/* Build and send hello. */
	var helloCmd helloCommand
	if err := helloCmd.build(myNonce, ephClientPrivkey, ephClientPubkey, permanentServerPubkey(permServerPubkey)); err != nil {
		if err == errNonceOverflow {
			return bail(newProtocolError("%s", err))
		}
		return bail(newInternalError("cannot build HELLO: %s", err))
	}

	if err := writeFrame(conn, &helloCmd); err != nil {
		return bail(err)
	}

	/* Receive and validate welcome. */
	var welcomeCmd welcomeCommand
	if err := readFrame(conn, &welcomeCmd); err != nil {
		return bail(err)
	}

	ephServerPubkey, sCookie, err := welcomeCmd.validate(ephClientPrivkey, permanentServerPubkey(permServerPubkey))
	if err != nil {
		return bail(newProtocolError("invalid WELCOME: %s", err))
	}

	/* Build and send initiate. */
	var initiateCmd initiateCommand
	if err := initiateCmd.build(myNonce,
		long_nonce,
		sCookie,
		permanentClientPrivkey(clientprivkey),
		permanentClientPubkey(clientpubkey),
		permanentServerPubkey(permServerPubkey),
		ephServerPubkey,
		ephClientPrivkey,
		ephClientPubkey); err != nil {
		if err == errNonceOverflow {
			return bail(newProtocolError("%s", err))
		}
		return bail(newInternalError("cannot build INITIATE: %s", err))
	}

	if err := writeFrame(conn, &initiateCmd); err != nil {
		return bail(err)
	}

	/* Receive and validate ready. */
	var genericCmd genericCommand
	if err := readFrame(conn, &genericCmd); err != nil {
		return bail(err)
	}

	specificCmd, err := genericCmd.convert()
	if err != nil {
		return bail(newProtocolError("invalid READY or ERROR: %s", err))
	}

	switch cmd := specificCmd.(type) {
	case *readyCommand:
		if err := cmd.validate(serverNonce, ephClientPrivkey, ephServerPubkey); err != nil {
			return bail(newProtocolError("invalid READY: %s", err))
		}
	case *errorCommand:
		reason, err := cmd.validate()
		if err != nil {
			return bail(newProtocolError("invalid ERROR: %s", err))
		}
		return bail(newAuthenticationError(reason))
	default:
		return bail(newProtocolError("invalid command: %s", cmd))
	}

	return &EncryptedConn{
		conn:        conn,
		myNonce:     myNonce,
		theirNonce:  serverNonce,
		myPrivkey:   Privkey(ephClientPrivkey),
		theirPubkey: Pubkey(ephServerPubkey),
		isServer:    false,
	}, nil
}

// Allow, when called on a server socket, signals the client that
// it is authorized to continue.
//
// It is an error to call Allow on a client socket.
//
// Lifecycle Information
//
// If Allow() returns an error, the passed socket will
// have been closed by the time this function returns.
func (c *EncryptedConn) Allow() error {
	bail := func(e error) error {
		// These are unrecoverable errors.  We close the socket.
		c.conn.Close()
		return e
	}

	/* Build and send ready. */
	var readyCmd readyCommand
	if err := readyCmd.build(c.myNonce,
		ephemeralServerPrivkey(c.myPrivkey),
		ephemeralClientPubkey(c.theirPubkey)); err != nil {
		if err == errNonceOverflow {
			return bail(newProtocolError("%s", err))
		}
		return bail(newInternalError("cannot build READY: %s", err))
	}

	if err := writeFrame(c.conn, &readyCmd); err != nil {
		return bail(err)
	}

	return nil
}

// Deny, when called on a server socket, signals the client that
// it is not authorized to continue, and closes the socket.
//
// It is an error to call Deny on a server socket.
//
// Lifecycle Information
//
// If Deny() returns an error, the passed socket will
// have been closed by the time this function returns.
//
// When Deny() returns normally, the underlying socket will have
// been closed too.
//
// Clients which receive a Deny() denial SHALL NOT reconnect with
// the same credentials, but wise implementors know that hostile
// clients can do what they want, so they will need to implement
// throttling based on public key.  WrapServer() returns the
// verified public key of the client before the server has made
// an authentication policy decision, so the server can implement
// throttling based on client public key.
func (c *EncryptedConn) Deny() error {
	bail := func(e error) error {
		// These are unrecoverable errors.  We close the socket.
		c.conn.Close()
		return e
	}

	/* Build and send error. */
	var errorCmd errorCommand
	if err := errorCmd.build("unauthorized"); err != nil {
		if err == errNonceOverflow {
			return bail(newProtocolError("%s", err))
		}
		return bail(newInternalError("cannot build ERROR: %s", err))
	}

	if err := writeFrame(c.conn, &errorCmd); err != nil {
		return bail(err)
	}

	err := c.conn.Close()
	return err
}

// IsAuthenticationError returns true when the error returned by
// WrapClient() was caused by the server rejecting the client
// for authentication reasons with Deny().
func IsAuthenticationError(e error) bool {
	_, ok := e.(*authenticationError)
	return ok
}
