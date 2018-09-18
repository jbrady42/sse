/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"sync"

	backoff "gopkg.in/cenkalti/backoff.v1"
)

var (
	headerID    = []byte("id:")
	headerData  = []byte("data:")
	headerEvent = []byte("event:")
	headerError = []byte("error:")
)

// Client handles an incoming server stream
type Client struct {
	URL            string
	Connection     *http.Client
	Headers        map[string]string
	EncodingBase64 bool
	EventID        string
	subscribed     map[chan *Event]chan bool

	sync.Mutex
}

// NewClient creates a new client
func NewClient(url string) *Client {
	return &Client{
		URL:        url,
		Connection: &http.Client{},
		Headers:    make(map[string]string),
		subscribed: make(map[chan *Event]chan bool),
	}
}

// Subscribe to a data stream
func (c *Client) Subscribe(stream string, handler func(msg *Event)) error {
	operation := func() error {
		resp, err := c.request(stream)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)

		for {
			// Read each new line and process the type of event
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return err
			}
			msg := c.processEvent(line)
			if msg != nil {
				handler(msg)
			}
		}
	}
	return backoff.Retry(operation, backoff.NewExponentialBackOff())
}

// SubscribeChan sends all events to the provided channel
func (c *Client) SubscribeChan(stream string, ch chan *Event) error {
	c.subscribed[ch] = make(chan bool)

	operation := func() error {
		resp, err := c.request(stream)
		if err != nil {
			c.cleanup(resp, ch)
			return err
		}

		if resp.StatusCode != 200 {
			c.cleanup(resp, ch)
			return errors.New("could not connect to stream")
		}

		reader := bufio.NewReader(resp.Body)

		go func() {
			for {
				// Read each new line and process the type of event
				line, err := reader.ReadBytes('\n')
				if err != nil {
					c.cleanup(resp, ch)
					return
				}

				msg := c.processEvent(line)
				if msg == nil {
					continue
				}

				select {
				case <-c.subscribed[ch]:
					c.cleanup(resp, ch)
					return
				case ch <- msg:
					// message sent
				}
			}
		}()

		return nil
	}

	return backoff.Retry(operation, backoff.NewExponentialBackOff())
}

// SubscribeRaw to an sse endpoint
func (c *Client) SubscribeRaw(handler func(msg *Event)) error {
	return c.Subscribe("", handler)
}

// SubscribeChanRaw sends all events to the provided channel
func (c *Client) SubscribeChanRaw(ch chan *Event) error {
	return c.SubscribeChan("", ch)
}

// Unsubscribe : unsubscribes a channel
func (c *Client) Unsubscribe(ch chan *Event) {
	c.Lock()
	defer c.Unlock()

	if c.subscribed[ch] != nil {
		c.subscribed[ch] <- true
	}
}

func (c *Client) request(stream string) (*http.Response, error) {
	req, err := http.NewRequest("GET", c.URL, nil)
	if err != nil {
		return nil, err
	}

	// Setup request, specify stream to connect to
	if stream != "" {
		query := req.URL.Query()
		query.Add("stream", stream)
		req.URL.RawQuery = query.Encode()
	}

	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "keep-alive")

	if c.EventID != "" {
		req.Header.Set("Last-Event-ID", c.EventID)
	}

	// Add user specified headers
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}

	return c.Connection.Do(req)
}

func (c *Client) processEvent(msg []byte) *Event {
	var e Event

	switch h := msg; {
	case bytes.Contains(h, headerID):
		e.ID = trimHeader(len(headerID), msg)
	case bytes.Contains(h, headerData):
		e.Data = trimHeader(len(headerData), msg)
	case bytes.Contains(h, headerEvent):
		e.Event = trimHeader(len(headerEvent), msg)
	case bytes.Contains(h, headerError):
		e.Error = trimHeader(len(headerError), msg)
	default:
		return nil
	}

	if len(e.Data) > 0 && c.EncodingBase64 {
		buf := make([]byte, base64.StdEncoding.DecodedLen(len(e.Data)))

		_, err := base64.StdEncoding.Decode(buf, e.Data)
		if err != nil {
			log.Println(err)
		}

		e.Data = buf
	}

	return &e
}

func (c *Client) cleanup(resp *http.Response, ch chan *Event) {
	if resp != nil {
		resp.Body.Close()
	}

	c.Lock()
	defer c.Unlock()

	if c.subscribed[ch] != nil {
		close(c.subscribed[ch])
		close(ch)
		delete(c.subscribed, ch)
	}
}

func trimHeader(size int, data []byte) []byte {
	data = data[size:]
	// Remove optional leading whitespace
	if data[0] == 32 {
		data = data[1:]
	}
	// Remove trailing new line
	if data[len(data)-1] == 10 {
		data = data[:len(data)-1]
	}
	return data
}
