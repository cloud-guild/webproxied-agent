package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/v1/report" || r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["agent_version"] != "v1.0.0" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Write([]byte(`{"server_id":7,"interval_seconds":60,"auto_update":false}`))
	}))
	defer server.Close()

	res, err := New(server.URL, "secret", "test").Report(context.Background(), map[string]string{"agent_version": "v1.0.0"})
	if err != nil || res.ServerID != 7 || res.AutoUpdate == nil || *res.AutoUpdate {
		t.Fatalf("unexpected: %+v %v", res, err)
	}

	_, err = New(server.URL, "wrong", "test").Report(context.Background(), map[string]string{})
	if !errors.As(err, &ErrUnauthorized{}) {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

func TestResponseWatchFields(t *testing.T) {
	var absent, cleared, set Response
	json.Unmarshal([]byte(`{"server_id":1}`), &absent)
	json.Unmarshal([]byte(`{"watch_services":[],"access_logs":[]}`), &cleared)
	json.Unmarshal([]byte(`{"inventory_due":true,"watch_services":["nginx"],"access_logs":[{"path":"/var/log/nginx/access.log","format":"combined"}]}`), &set)

	// 応答に無ければ今の指定のまま（nil）、空の配列なら監視をやめる
	if absent.WatchServices != nil || absent.AccessLogs != nil || absent.InventoryDue {
		t.Errorf("absent: %+v", absent)
	}
	if cleared.WatchServices == nil || len(cleared.WatchServices) != 0 || cleared.AccessLogs == nil {
		t.Errorf("cleared: %+v", cleared)
	}
	if !set.InventoryDue || set.WatchServices[0] != "nginx" || set.AccessLogs[0].Format != "combined" {
		t.Errorf("set: %+v", set)
	}
}
