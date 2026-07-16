package xui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/xboard-bridge/xboard-xui-bridge/internal/config"
)

func TestGetClientIPsDecodesSupportedShapes(t *testing.T) {
	tests := []struct {
		name string
		obj  string
		want []string
	}{
		{
			name: "legacy string array",
			obj:  `["1.1.1.1 (2026-07-16 12:00:00)","2.2.2.2"]`,
			want: []string{"1.1.1.1 (2026-07-16 12:00:00)", "2.2.2.2"},
		},
		{
			name: "current object array",
			obj:  `[{"ip":"1.1.1.1","time":"2026-07-16 12:00:00","node":"edge-1"},{"ip":"2.2.2.2","time":"","node":""}]`,
			want: []string{"1.1.1.1", "2.2.2.2"},
		},
		{
			name: "mixed array",
			obj:  `["1.1.1.1 (2026-07-16 12:00:00)",{"ip":"2.2.2.2","time":"","node":""}]`,
			want: []string{"1.1.1.1 (2026-07-16 12:00:00)", "2.2.2.2"},
		},
		{
			name: "empty array",
			obj:  `[]`,
			want: []string{},
		},
		{
			name: "object array skips empty ip",
			obj:  `[{"ip":"","time":"","node":""},{"ip":"2.2.2.2","time":"","node":""}]`,
			want: []string{"2.2.2.2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, tt.obj)
			got, err := c.GetClientIPs(context.Background(), "user@example.com")
			if err != nil {
				t.Fatalf("GetClientIPs returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GetClientIPs = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestGetClientIPsDecodesNoIPShapes(t *testing.T) {
	tests := []struct {
		name string
		obj  string
	}{
		{name: "null", obj: `null`},
		{name: "no ip record", obj: `"No IP Record"`},
		{name: "missing obj", obj: ``},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, tt.obj)
			got, err := c.GetClientIPs(context.Background(), "user@example.com")
			if err != nil {
				t.Fatalf("GetClientIPs returned error: %v", err)
			}
			if got != nil {
				t.Fatalf("GetClientIPs = %#v, want nil", got)
			}
		})
	}
}

func TestGetClientIPsRejectsUnexpectedShapes(t *testing.T) {
	tests := []struct {
		name    string
		obj     string
		wantErr string
	}{
		{name: "unexpected string", obj: `"No records"`, wantErr: "意外字符串"},
		{name: "top-level object", obj: `{"ip":"1.1.1.1"}`, wantErr: "既非数组也非"},
		{name: "array number item", obj: `[1]`, wantErr: "既非字符串也非对象"},
		{name: "array bool item", obj: `[true]`, wantErr: "既非字符串也非对象"},
		{name: "array null item", obj: `[null]`, wantErr: "既非字符串也非对象"},
		{name: "object missing ip", obj: `[{"time":"2026-07-16 12:00:00","node":"edge-1"}]`, wantErr: "缺少 ip 字段"},
		{name: "object non-string ip", obj: `[{"ip":123,"time":"","node":""}]`, wantErr: "ip 字段无效"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, tt.obj)
			_, err := c.GetClientIPs(context.Background(), "user@example.com")
			if err == nil {
				t.Fatal("GetClientIPs returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("GetClientIPs error = %q, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func newTestClient(t *testing.T, obj string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/panel/api/clients/ips/user@example.com" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if obj == "" {
			_, _ = w.Write([]byte(`{"success":true,"msg":""}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"msg":"","obj":` + obj + `}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(config.Xui{
		APIHost:    srv.URL,
		APIToken:   "test-token",
		TimeoutSec: 5,
	}, nil)
	if err != nil {
		t.Fatalf("New xui client: %v", err)
	}
	return c
}
