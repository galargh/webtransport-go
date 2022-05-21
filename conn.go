package webtransport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
	"github.com/lucas-clemente/quic-go/quicvarint"
)

// sessionID is the WebTransport Session ID
type sessionID uint64

type Conn struct {
	sessionID  sessionID
	qconn      http3.StreamCreator
	requestStr io.Reader // TODO: this needs to be an io.ReadWriteCloser so we can close the stream

	streamHdr    []byte
	uniStreamHdr []byte

	ctx      context.Context
	closeErr error
	closed   chan struct{}

	// for bidirectional streams
	acceptMx   sync.Mutex
	acceptChan chan struct{}
	// Contains all the streams waiting to be accepted.
	// There's no explicit limit to the length of the queue, but it is implicitly
	// limited by the stream flow control provided by QUIC.
	acceptQueue []quic.Stream

	// for unidirectional streams
	acceptUniMx   sync.Mutex
	acceptUniChan chan struct{}
	// Contains all the streams waiting to be accepted.
	// There's no explicit limit to the length of the queue, but it is implicitly
	// limited by the stream flow control provided by QUIC.
	acceptUniQueue []quic.ReceiveStream
}

func newConn(sessionID sessionID, qconn http3.StreamCreator, requestStr io.Reader) *Conn {
	ctx, ctxCancel := context.WithCancel(context.Background())
	c := &Conn{
		sessionID:     sessionID,
		qconn:         qconn,
		requestStr:    requestStr,
		ctx:           ctx,
		closed:        make(chan struct{}),
		acceptChan:    make(chan struct{}, 1),
		acceptUniChan: make(chan struct{}, 1),
	}
	// precompute the headers for unidirectional streams
	buf := bytes.NewBuffer(make([]byte, 0, 2+quicvarint.Len(uint64(c.sessionID))))
	quicvarint.Write(buf, webTransportUniStreamType)
	quicvarint.Write(buf, uint64(c.sessionID))
	c.uniStreamHdr = buf.Bytes()
	// precompute the headers for bidirectional streams
	buf = bytes.NewBuffer(make([]byte, 0, 2+quicvarint.Len(uint64(c.sessionID))))
	quicvarint.Write(buf, webTransportFrameType)
	quicvarint.Write(buf, uint64(c.sessionID))
	c.streamHdr = buf.Bytes()

	go func() {
		defer ctxCancel()
		c.handleConn()
	}()
	return c
}

func (c *Conn) handleConn() {
	for {
		// TODO: parse capsules sent on the request stream
		b := make([]byte, 100)
		if _, err := c.requestStr.Read(b); err != nil {
			c.closeErr = fmt.Errorf("WebTransport session closed: %w", err)
			close(c.closed)
			return
		}
	}
}

func (c *Conn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *Conn) addStream(str quic.Stream) {
	c.acceptMx.Lock()
	defer c.acceptMx.Unlock()

	c.acceptQueue = append(c.acceptQueue, str)
	select {
	case c.acceptChan <- struct{}{}:
	default:
	}
}

func (c *Conn) addUniStream(str quic.ReceiveStream) {
	c.acceptUniMx.Lock()
	defer c.acceptUniMx.Unlock()

	c.acceptUniQueue = append(c.acceptUniQueue, str)
	select {
	case c.acceptUniChan <- struct{}{}:
	default:
	}
}

// Context returns a context that is closed when the connection is closed.
func (c *Conn) Context() context.Context {
	return c.ctx
}

func (c *Conn) AcceptStream(ctx context.Context) (Stream, error) {
	if c.isClosed() {
		return nil, c.closeErr
	}
	var str quic.Stream
	c.acceptMx.Lock()
	if len(c.acceptQueue) > 0 {
		str = c.acceptQueue[0]
		c.acceptQueue = c.acceptQueue[1:]
	}
	c.acceptMx.Unlock()
	if str != nil {
		return newStream(str, nil), nil
	}

	select {
	case <-c.ctx.Done():
		return nil, c.closeErr
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.acceptChan:
		return c.AcceptStream(ctx)
	}
}

func (c *Conn) AcceptUniStream(ctx context.Context) (ReceiveStream, error) {
	if c.isClosed() {
		return nil, c.closeErr
	}
	var str quic.ReceiveStream
	c.acceptUniMx.Lock()
	if len(c.acceptUniQueue) > 0 {
		str = c.acceptUniQueue[0]
		c.acceptUniQueue = c.acceptUniQueue[1:]
	}
	c.acceptUniMx.Unlock()
	if str != nil {
		return newReceiveStream(str), nil
	}

	select {
	case <-c.ctx.Done():
		return nil, c.closeErr
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.acceptUniChan:
		return c.AcceptUniStream(ctx)
	}
}

func (c *Conn) OpenStream() (Stream, error) {
	if c.isClosed() {
		return nil, c.closeErr
	}
	str, err := c.qconn.OpenStream()
	if err != nil {
		return nil, err
	}
	return newStream(str, c.streamHdr), nil
}

func (c *Conn) OpenStreamSync(ctx context.Context) (Stream, error) {
	if c.isClosed() {
		return nil, c.closeErr
	}
	str, err := c.qconn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return newStream(str, c.streamHdr), nil
}

func (c *Conn) OpenUniStream() (SendStream, error) {
	if c.isClosed() {
		return nil, c.closeErr
	}
	str, err := c.qconn.OpenUniStream()
	if err != nil {
		return nil, err
	}
	return newSendStream(str, c.uniStreamHdr), nil
}

func (c *Conn) OpenUniStreamSync(ctx context.Context) (SendStream, error) {
	if c.isClosed() {
		return nil, c.closeErr
	}
	str, err := c.qconn.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return newSendStream(str, c.uniStreamHdr), nil
}

func (c *Conn) LocalAddr() net.Addr {
	return c.qconn.LocalAddr()
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.qconn.RemoteAddr()
}

func (c *Conn) Close() error {
	return nil
}
