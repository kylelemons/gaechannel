package gaechannel

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"
)

type devChannel struct {
	commonChannel
	pollWait time.Duration
	stop     chan bool
	done     chan bool
}

func newDevChannel(common *commonChannel) *devChannel {
	return &devChannel{
		commonChannel: *common,
		pollWait:      500 * time.Millisecond,
		stop:          make(chan bool),
		done:          make(chan bool),
	}
}

func (c *devChannel) commandURL(command string) string {
	v := url.Values{
		"command": {command},
		"channel": {c.Token},
		"client":  {c.ClientID},
	}
	u := url.URL{
		Scheme:   "http",
		Host:     c.Host,
		Path:     path.Join(c.ChannelPath, "dev"),
		RawQuery: v.Encode(),
	}
	return u.String()
}

func (c *devChannel) open() error {
	_, err := http.Get(c.commandURL("connect"))
	// resp.Header.Get("Server") == "Development/1.0"
	return err
}

func (c *devChannel) poll(data chan<- string) error {
	defer close(c.done)

	// I don't think this is really correct,
	// as it seems to wait until it times out
	// when there's actually a message...
	send := func(body io.ReadCloser) error {
		defer body.Close()
		buf := new(bytes.Buffer)
		if _, err := io.Copy(buf, body); err != nil {
			return err
		}
		data <- buf.String()
		return nil
	}

	for {
		select {
		case <-c.stop:
			return nil
		default:
		}

		resp, err := http.Get(c.commandURL("poll"))
		if err != nil {
			return err
		}
		body := resp.Body
		resp.Body = nil

		if resp.ContentLength > 0 {
			if err := send(body); err != nil {
				return err
			}
		} else {
			fmt.Printf(".")
			time.Sleep(c.pollWait)
			body.Close()
		}
	}
	return nil
}

func (c *devChannel) Stream(data chan<- string) error {
	if err := c.open(); err != nil {
		return err
	}
	return c.poll(data)
}

func (c *devChannel) Close() error {
	close(c.stop)
	<-c.done
	_, err := http.Get(c.commandURL("disconnect"))
	return err
}
