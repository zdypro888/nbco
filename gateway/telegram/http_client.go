package telegram

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// localBotAPIHTTPClient normalizes the empty multipart requests emitted by
// github.com/go-telegram/bot for parameterless methods. Telegram's cloud API
// accepts an empty chunked multipart body, while the official local Bot API
// correctly rejects the malformed body. Non-empty bodies remain streaming.
type localBotAPIHTTPClient struct {
	client *http.Client
}

func newLocalBotAPIHTTPClient() *localBotAPIHTTPClient {
	return &localBotAPIHTTPClient{client: &http.Client{Timeout: time.Minute}}
}

func (c *localBotAPIHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if req.Body == nil || req.Body == http.NoBody ||
		!strings.HasPrefix(strings.ToLower(req.Header.Get("Content-Type")), "multipart/form-data;") {
		return c.client.Do(req)
	}

	body := req.Body
	buffered := bufio.NewReaderSize(body, 1)
	if _, err := buffered.Peek(1); err != nil {
		if !errors.Is(err, io.EOF) {
			_ = body.Close()
			return nil, err
		}
		_ = body.Close()
		req.Body = http.NoBody
		req.ContentLength = 0
		req.TransferEncoding = nil
		req.Trailer = nil
		return c.client.Do(req)
	}

	req.Body = struct {
		io.Reader
		io.Closer
	}{Reader: buffered, Closer: body}
	return c.client.Do(req)
}
