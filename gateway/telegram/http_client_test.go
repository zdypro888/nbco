package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
)

func TestLocalBotAPIHTTPClientNormalizesEmptyMultipartAndStreamsParameters(t *testing.T) {
	var sawGetMe, sawSendMessage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bot123:secret/getMe":
			sawGetMe = true
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read getMe body: %v", err)
			}
			if len(body) != 0 {
				t.Errorf("getMe body length = %d, want 0", len(body))
			}
			if len(r.TransferEncoding) != 0 {
				t.Errorf("getMe transfer encoding = %v, want none", r.TransferEncoding)
			}
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"nbco","username":"nbco_bot"}}`)
		case "/bot123:secret/sendMessage":
			sawSendMessage = true
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse sendMessage multipart: %v", err)
			}
			if got := r.FormValue("chat_id"); got != "42" {
				t.Errorf("chat_id = %q, want 42", got)
			}
			if got := r.FormValue("text"); got != "hello" {
				t.Errorf("text = %q, want hello", got)
			}
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":42,"type":"private"},"text":"hello"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b, err := bot.New("123:secret",
		bot.WithServerURL(srv.URL),
		bot.WithHTTPClient(time.Minute, newLocalBotAPIHTTPClient()),
	)
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}
	if _, err := b.SendMessage(context.Background(), &bot.SendMessageParams{ChatID: "42", Text: "hello"}); err != nil {
		t.Fatalf("send message: %v", err)
	}
	if !sawGetMe || !sawSendMessage {
		t.Fatalf("requests observed: getMe=%t sendMessage=%t", sawGetMe, sawSendMessage)
	}
}

func TestLocalBotAPIHTTPClientLeavesOtherContentTypesAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got := string(body); got != "payload" {
			t.Errorf("body = %q, want payload", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := newLocalBotAPIHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
