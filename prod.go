package gaechannel

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type prodChannel struct {
	commonChannel
	talkHost string
	talkPath string

	gClientID  string
	gSessionID string

	sid string
	rid int
	mid int64

	backoff time.Duration
	backMax time.Duration

	stop chan bool
	done chan bool
}

func newProdChannel(common *commonChannel) *prodChannel {
	return &prodChannel{
		commonChannel: *common,
		talkHost:      "talkgadget.google.com",
		talkPath:      "/talkgadget/",
		mid:           1,
		backoff:       100 * time.Millisecond,
		backMax:       1 * time.Minute,
		stop:          make(chan bool),
		done:          make(chan bool),
	}
}

var callExtract = regexp.MustCompile(`(?ms)chat\.WcsDataClient\(([^\)]+)\)`)

func (c *prodChannel) initialize() error {
	crossPageConfig := map[string]string{
		"cn":  randLetters(10),
		"tp":  "null",
		"lpu": "https://" + c.talkHost + path.Join(c.talkPath, "xpc_blank"),
		"ppu": "http://" + c.Host + path.Join(c.ChannelPath, "xpc_blank"),
	}
	// This cannot fail because it's a map[string]string
	xpc, _ := json.Marshal(crossPageConfig)
	initURL := (&url.URL{
		Scheme: "https",
		Host:   c.talkHost,
		Path:   path.Join(c.talkPath, "d"),
		RawQuery: url.Values{
			"token": {c.Token},
			"xpc":   {string(xpc)},
		}.Encode(),
	}).String()

	resp, err := http.Get(initURL)
	if err != nil {
		return fmt.Errorf("failed to initialize talk: %s", err)
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read body: %s", err)
	}

	groups := callExtract.FindStringSubmatch(string(data))
	if groups == nil {
		return fmt.Errorf("failed to find setup call")
	}
	lines := strings.Split(groups[1], "\n")
	var params []string
	for i, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimRight(line, ",")
		if len(line) == 0 {
			continue
		}

		param, err := strconv.Unquote(line)
		if err != nil {
			return fmt.Errorf("failed to unquote line %d: %s", i, err)
		}
		params = append(params, param)
	}
	if got, want := len(params), 7; got != want {
		return fmt.Errorf("incorrect params, got %d, want %d", got, want)
	}
	c.gClientID = params[2]
	c.gSessionID = params[3]
	if got, want := params[6], c.Token; got != want {
		return fmt.Errorf("wcs token = %q, want %q", got, want)
	}
	log.Printf("gaechannel: Talk session %q registered with clid %q", c.gSessionID, c.gClientID)

	return nil
}

func (c *prodChannel) bindURL(extra url.Values) string {
	v := url.Values{
		"VER":        {"8"},
		"RID":        {strconv.Itoa(c.rid)},
		"token":      {c.Token},
		"gsessionid": {c.gSessionID},
		"clid":       {c.gClientID},
		"prop":       {"data"},
		"zx":         {randLetters(12)},
		"t":          {"1"},
	}
	for key, vals := range extra {
		v[key] = vals
	}
	if c.sid != "" {
		v.Add("SID", c.sid)
	}
	c.rid++

	u := url.URL{
		Scheme:   "https",
		Host:     c.talkHost,
		Path:     path.Join(c.talkPath, "dch/bind"),
		RawQuery: v.Encode(),
	}
	return u.String()
}

func (c *prodChannel) fetchSID() error {
	bind := c.bindURL(url.Values{
		"CVER": {"1"},
	})
	data := url.Values{
		"count": {"0"},
	}
	resp, err := http.PostForm(bind, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read body: %s", err)
	}
	raw = bytes.Replace(raw, []byte(",,"), []byte(`,"",`), -1)
	raw = bytes.Replace(raw, []byte{'\''}, []byte{'"'}, -1)
	raw = raw[bytes.IndexByte(raw, '['):]

	var msg []interface{}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return fmt.Errorf("unable to decode %q: %s", raw, err)
	}
	if err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("recovered: %v", r)
			}
		}()

		first := msg[0].([]interface{})
		list := first[1].([]interface{})
		key, val := list[0].(string), list[1].(string)
		if got, want := key, "c"; got != want {
			return fmt.Errorf("item 0 key = %q, want %q", got, want)
		}
		c.sid = val
		return nil
	}(); err != nil {
		return fmt.Errorf("unable to extract sid from %q: %s", err)
	}
	log.Printf("gaechannel: Received SID %q", c.sid)

	return nil
}

func (c *prodChannel) connect() error {
	bind := c.bindURL(url.Values{
		"CVER": {"1"},
		"AID":  {fmt.Sprintf("%d", c.mid)},
	})
	params := url.Values{
		"count":    {"1"},
		"ofs":      {"0"},
		"req0_m":   {`["connect-add-client"]`},
		"req0_c":   {c.gClientID},
		"req0__sc": {"c"},
	}
	if _, err := http.PostForm(bind, params); err != nil {
		return fmt.Errorf("connect: %s", err)
	}
	log.Printf("gaechannel: Connected")
	return nil
}

func (c *prodChannel) poll(data chan<- string) error {
	defer close(c.done)
	backoff := c.backoff

	buf := new(bytes.Buffer)
	decode := func(body io.ReadCloser) error {
		defer body.Close()

		r := bufio.NewReader(body)
		for {
			buf.Reset()

			// Read packet length
			line, err := r.ReadString('\n')
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("error looking for size: %s", err)
			}
			log.Printf("gaechannel: Reading packet...")

			line = strings.TrimSpace(line)
			size, err := strconv.Atoi(line)
			if err != nil {
				return fmt.Errorf("size %q is not a valid number: %s", line, err)
			}

			// Read packet
			if _, err := io.CopyN(buf, r, int64(size)); err != nil {
				return fmt.Errorf("reading packet: %s", err)
			}

			raw := buf.Bytes()
			for bytes.Index(raw, []byte(",,")) >= 0 {
				raw = bytes.Replace(raw, []byte(",,"), []byte(`,"",`), -1)
			}
			raw = bytes.Replace(raw, []byte{'\''}, []byte{'"'}, -1)
			raw = raw[bytes.IndexByte(raw, '['):]

			var packet []interface{}
			if err := json.Unmarshal(raw, &packet); err != nil {
				return fmt.Errorf("unable to decode %q: %s", raw, err)
			}

			log.Printf("gaechannel: Examining %d messages...", len(packet))
			for _, ipair := range packet {
				pair, ok := ipair.([]interface{})
				if ok && len(pair) == 2 {
					// Decode message number
					msgID, ok := pair[0].(float64)
					if !ok {
						log.Printf("gaechannel: msgID is not a number? %T", pair[0])
					}
					c.mid = int64(msgID)

					// Decode the message type
					msg, ok := pair[1].([]interface{})
					if !ok {
						log.Printf("gaechannel: msg is not a list? %T", pair[1])
					}
					if len(msg) < 2 {
						continue
					}
					if kind, ok := msg[0].(string); !ok || kind != "c" {
						continue
					}

					// Extract the C message payload
					cmsg, ok := msg[1].([]interface{})
					if !ok || len(cmsg) < 2 {
						continue
					}
					payload, ok := cmsg[1].([]interface{})
					if !ok || len(payload) < 2 {
						continue
					}

					// Check the payload type
					if kind, ok := payload[0].(string); !ok || kind != "ae" {
						continue
					}

					log.Printf("gaechannel: Incoming message: %q", payload[1])
					data <- payload[1].(string)
					backoff = c.backoff
				}
			}
		}
		return nil
	}

	for {
		select {
		case <-c.stop:
			return nil
		default:
		}

		log.Printf("gaechannel: Waiting %v...", backoff)
		time.Sleep(backoff)
		if backoff *= 2; backoff > c.backMax {
			backoff = c.backMax
		}

		bind := c.bindURL(url.Values{
			"CI":   {"0"},
			"AID":  {fmt.Sprintf("%d", c.mid)},
			"TYPE": {"xmlhttp"},
			"RID":  {"rpc"},
		})
		log.Printf("gaechannel: Polling...")
		resp, err := http.Get(bind)
		if err != nil {
			return fmt.Errorf("poll: %s", err)
		}
		if resp.StatusCode/100 != 2 {
			buf.Reset()
			io.Copy(buf, resp.Body)
			resp.Body.Close()
			log.Printf("gaechannel: Poll error %d:\n%s", resp.StatusCode, buf.String())
			switch resp.StatusCode {
			case http.StatusBadRequest, http.StatusUnauthorized:
				return ErrReauth
			}
			continue
		}
		if err := decode(resp.Body); err != nil {
			log.Printf("gaechannel: Decoding error: %s", err)
			continue
		}
		backoff = c.backoff
	}
	return nil
}

func (c *prodChannel) Stream(data chan<- string) error {
	if err := c.initialize(); err != nil {
		return err
	}
	if err := c.fetchSID(); err != nil {
		return err
	}
	if err := c.connect(); err != nil {
		return err
	}
	return c.poll(data)
}

func randLetters(size int) string {
	random := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		log.Fatalf("gaechannel: could not generate random number: %s", err)
	}
	/*
		for i, v := range random {
			base := byte('a')
			if v%2 > 0 {
				base = 'A'
			}
			v /= 2
			random[i] = base + v%26
		}
	*/
	return base64.URLEncoding.EncodeToString(random)[:size]
}

func (c *prodChannel) Close() error {
	close(c.stop)
	<-c.done
	return nil
}
