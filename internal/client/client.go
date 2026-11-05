// Package client は、アプリ（WebProxied）へ、集めたデータをHTTPSで送る。サーバー側で受信用のポートを開ける必要はない。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client は、アプリのAPIの呼び出し。
type Client struct {
	APIURL    string
	Token     string
	UserAgent string
	HTTP      *http.Client
}

// Response は、アプリからの応答（次の送信の間隔や、自動更新の設定）。
type Response struct {
	ServerID        int64 `json:"server_id"`
	IntervalSeconds int   `json:"interval_seconds"`
	AutoUpdate      *bool `json:"auto_update"`
}

// ErrUnauthorized は、トークンが無効なとき（サーバーの登録が削除された・トークンを作り直した）。
type ErrUnauthorized struct{}

func (ErrUnauthorized) Error() string {
	return "トークンが無効です（サーバーの登録を確認し、新しいトークンで設定し直してください）"
}

// New は、クライアントを作る。
func New(apiURL, token, userAgent string) *Client {
	return &Client{APIURL: apiURL, Token: token, UserAgent: userAgent, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Report は、データを送る。
func (c *Client) Report(ctx context.Context, payload any) (*Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIURL+"/api/agent/v1/report", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)

	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return nil, ErrUnauthorized{}
	case res.StatusCode < 200 || res.StatusCode >= 300:
		return nil, fmt.Errorf("アプリが %d を返しました", res.StatusCode)
	}

	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("応答を読めませんでした: %w", err)
	}

	return &out, nil
}
