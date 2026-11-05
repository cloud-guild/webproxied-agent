// Package config は、エージェントの設定（/etc/webproxied-agent/agent.conf）を読む。
// 形式は「KEY=VALUE」の行（#で始まる行は注釈）。トークンを含むため、ファイルはroot（とエージェントのユーザー）だけが読めるようにする。
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultPath は、設定ファイルの既定の場所。
const DefaultPath = "/etc/webproxied-agent/agent.conf"

// Config は、エージェントの設定。
type Config struct {
	// APIURL は、アプリのURL（例: https://webproxied.com）。
	APIURL string
	// Token は、サーバーごとに発行する認証用のトークン。
	Token string
	// Interval は、データを集めて送る間隔。
	Interval time.Duration
	// AutoUpdate は、署名を検証できた新しい版へ、自動で更新するか。
	AutoUpdate bool
}

// Load は、設定ファイルを読む。
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return Parse(f)
}

// Parse は、設定の内容を読む。
func Parse(r interface{ Read([]byte) (int, error) }) (*Config, error) {
	cfg := &Config{Interval: time.Minute, AutoUpdate: true}
	scanner := bufio.NewScanner(r)

	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("%d行目: KEY=VALUEの形式ではありません", line)
		}

		key, value = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), `"'`)

		switch key {
		case "API_URL":
			cfg.APIURL = strings.TrimRight(value, "/")
		case "TOKEN":
			cfg.Token = value
		case "INTERVAL_SECONDS":
			seconds, err := strconv.Atoi(value)
			if err != nil || seconds < 30 {
				return nil, fmt.Errorf("%d行目: INTERVAL_SECONDSは30以上の整数にしてください", line)
			}
			cfg.Interval = time.Duration(seconds) * time.Second
		case "AUTO_UPDATE":
			cfg.AutoUpdate = value != "false" && value != "0" && value != "no"
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return cfg, cfg.validate()
}

func (c *Config) validate() error {
	if c.Token == "" {
		return errors.New("TOKENが設定されていません")
	}

	u, err := url.Parse(c.APIURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("API_URLは https:// で始まるURLにしてください")
	}

	return nil
}
