package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/moov-io/iso8583"
	"github.com/moov-io/iso8583/network"
)

var ErrConnectionClosed = errors.New("connection closed")

type Client struct {
	conn       net.Conn
	requestsCh chan request
	respMap    map[string]chan *iso8583.Message
	mutex      sync.Mutex // to protect following
	closing    bool       // user has called Close
	stan       int32      // STAN counter, max can be 999999
}

func NewClient() *Client {
	return &Client{
		requestsCh: make(chan request),
		respMap:    make(map[string]chan *iso8583.Message),
	}
}

func (c *Client) Connect(addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("connecting to server: %v", err)
	}
	c.conn = conn

	go c.writeLoop()
	go c.readLoop()

	return nil
}

func (c *Client) Close() error {
	c.mutex.Lock()
	// if we are closing already, return error
	c.closing = true
	c.mutex.Unlock()

	return c.conn.Close()
}

type request struct {
	rawMessage []byte // includes length header and message itself
	requestID  string
	replyCh    chan *iso8583.Message
	errCh      chan error
}

// send message and waits for the response
func (c *Client) Send(message *iso8583.Message) (*iso8583.Message, error) {
	c.mutex.Lock()
	if c.closing {
		c.mutex.Unlock()
		return nil, ErrConnectionClosed
	}
	c.mutex.Unlock()

	// prepare message for sending

	// set STAN if it's empty
	err := c.setMessageSTAN(message)
	if err != nil {
		return nil, fmt.Errorf("setting message STAN: %v", err)
	}

	var buf bytes.Buffer
	packed, err := message.Pack()
	if err != nil {
		return nil, fmt.Errorf("packing message: %v", err)
	}

	// create header
	header := network.NewVMLHeader()
	header.SetLength(len(packed))

	_, err = header.WriteTo(&buf)
	if err != nil {
		return nil, fmt.Errorf("writing message header: %v", err)
	}

	_, err = buf.Write(packed)
	if err != nil {
		return nil, fmt.Errorf("writing packed message to buffer: %v", err)
	}

	// prepare request
	reqID, err := requestID(message)
	if err != nil {
		return nil, fmt.Errorf("getting request ID: %v", err)
	}

	req := request{
		rawMessage: buf.Bytes(),
		requestID:  reqID,
		replyCh:    make(chan *iso8583.Message),
		errCh:      make(chan error),
	}

	var resp *iso8583.Message

	c.requestsCh <- req

	select {
	// we can add timeout here as well
	// ...
	case resp = <-req.replyCh:
	case err = <-req.errCh:
	}

	return resp, err
}

func (c *Client) setMessageSTAN(message *iso8583.Message) error {
	stan, err := message.GetString(11)
	if err != nil {
		return fmt.Errorf("getting STAN (field 11) of the message: %v", err)
	}

	// no STAN was provided, generate a new one
	if stan == "" {
		stan = c.getSTAN()
	}

	err = message.Field(11, stan)
	if err != nil {
		return fmt.Errorf("setting STAN (field 11): %s of the message: %v", stan, err)
	}

	return nil
}

// request id should be generated using different message fields (STAN, RRN, etc.)
// each request/response should be uniquely linked to the message
// current assumption is that STAN should be enough for this
// but because STAN is 6 digits, there is no way we can process millions transactions
// per second using STAN only
// More options for STAN:
// * match by RRN + STAN
// * it's typically unique in 24h and usually scoped to TID and transmission time fields.
func requestID(message *iso8583.Message) (string, error) {
	stan, err := message.GetString(11)
	if err != nil {
		return "", fmt.Errorf("getting STAN (field 11) of the message: %v", err)
	}
	return stan, nil
}

// TODO: when do we return from this goroutine?
func (c *Client) writeLoop() {
	// TODO
	// we should either (select)
	// * send heartbeat message
	// * read request from requestsCh
	// * if client was closed, reject all outstanding requests and return
	for req := range c.requestsCh {
		// TODO we should lock here before modifying a map
		c.respMap[req.requestID] = req.replyCh

		_, err := c.conn.Write([]byte(req.rawMessage))
		if err != nil {
			req.errCh <- err
			// TODO: delete request from respMap + with mutext
			// TODO: handle write error: reconnect? shutdown? panic?
		}
	}
}

// TODO: when do we return from this goroutine
func (c *Client) readLoop() {
	// TODO
	// read messages from the connection
	// if we got error during reading, what should we do? should we reconnect?
	// if client was closed, set timeout and wait for all pending requests to be replied and return
	var err error

	r := bufio.NewReader(c.conn)
	for {
		// read header first
		header := network.NewVMLHeader()
		_, err := header.ReadFrom(r)
		if err != nil {
			break
		}

		// read the packed message
		raw := make([]byte, header.Length())
		_, err = io.ReadFull(r, raw)
		if err != nil {
			break
		}

		// create message
		message := iso8583.NewMessage(brandSpec)
		err = message.Unpack(raw)
		if err != nil {
			break
		}

		reqID, err := requestID(message)
		if err != nil {
			break
		}

		// send response message to the reply channel
		if replyCh, found := c.respMap[reqID]; found {
			replyCh <- message
			// TODO: this one should be done inside mutex lock
			delete(c.respMap, reqID)
		} else {
			// we should log information about received message as
			// there is no one to give it to. Maybe create a lost
			// message queue?
		}
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	// if we receive error and we are closing connection, we have to set
	// err to ErrConnectionClosed otherwise just use err itself this if
	// should be reworked when we remove scanner and replace it with
	// reading from network
	if err != nil && !c.closing {
		fmt.Fprintln(os.Stderr, "reading standard input:", err)
	}

	// we should send err to all outstanding (pending) requests
}

// Some assumptions:
// * We can use the same STAN after request/response messages for such STAN were handled
// * STAN can be incremented but it MAX is 999999 it means we can start from 0 when we reached max
func (c *Client) getSTAN() string {
	// TODO: maybe use own mutex
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.stan++
	if c.stan > 999999 {
		c.stan = 0
	}
	return fmt.Sprintf("%06d", c.stan)
}
