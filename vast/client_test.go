package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListInstances(t *testing.T) {
	want := []Instance{
		{ID: 1, ActualStatus: "running", GPUName: "RTX4090"},
		{ID: 2, ActualStatus: "running", GPUName: "A100"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/instances/" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		json.NewEncoder(w).Encode(InstancesResponse{Instances: want})
	}))
	defer srv.Close()

	c := newTestClient("test-key", srv.URL)
	instances, err := c.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances() error: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("got %d instances, want 2", len(instances))
	}
	if instances[0].ID != 1 || instances[1].ID != 2 {
		t.Errorf("got IDs %d, %d", instances[0].ID, instances[1].ID)
	}
}

func TestListInstancesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	_, err := c.ListInstances(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestAttachSSHKey(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/instances/42/ssh/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	err := c.AttachSSHKey(context.Background(), 42, "ssh-rsa AAAA...")
	if err != nil {
		t.Fatalf("AttachSSHKey() error: %v", err)
	}
	if gotBody["ssh_key"] != "ssh-rsa AAAA..." {
		t.Errorf("ssh_key = %q", gotBody["ssh_key"])
	}
}

func TestAttachSSHKeyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	err := c.AttachSSHKey(context.Background(), 1, "ssh-rsa AAAA...")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestListInstancesDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not valid json`))
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	_, err := c.ListInstances(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestListInstancesNetworkError(t *testing.T) {
	// Use a server that's already closed to get a network error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := newTestClient("key", srv.URL)
	_, err := c.ListInstances(context.Background())
	if err == nil {
		t.Fatal("expected error for closed server")
	}
}

func TestAttachSSHKeyNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := newTestClient("key", srv.URL)
	err := c.AttachSSHKey(context.Background(), 1, "ssh-rsa AAAA...")
	if err == nil {
		t.Fatal("expected error for closed server")
	}
}

func TestDestroyInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/instances/42/" {
			t.Errorf("path = %s, want /instances/42/", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	c := newTestClient("test-key", srv.URL)
	err := c.DestroyInstance(context.Background(), 42)
	if err != nil {
		t.Fatalf("DestroyInstance() error: %v", err)
	}
}

func TestDestroyInstanceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	err := c.DestroyInstance(context.Background(), 999)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestDestroyInstanceNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := newTestClient("key", srv.URL)
	err := c.DestroyInstance(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error for closed server")
	}
}

func TestSetLabel(t *testing.T) {
	var gotMethod string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		if r.URL.Path != "/instances/42/" {
			t.Errorf("path = %s, want /instances/42/", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient("test-key", srv.URL)
	err := c.SetLabel(context.Background(), 42, "proxied")
	if err != nil {
		t.Fatalf("SetLabel() error: %v", err)
	}
	if gotMethod != "PUT" {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if gotBody["label"] != "proxied" {
		t.Errorf("label = %q, want proxied", gotBody["label"])
	}
}

func TestSetLabelClear(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	err := c.SetLabel(context.Background(), 1, "")
	if err != nil {
		t.Fatalf("SetLabel() error: %v", err)
	}
	if gotBody["label"] != "" {
		t.Errorf("label = %q, want empty", gotBody["label"])
	}
}

func TestSetLabelHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	err := c.SetLabel(context.Background(), 1, "proxied")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestSetLabelNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := newTestClient("key", srv.URL)
	err := c.SetLabel(context.Background(), 1, "proxied")
	if err == nil {
		t.Fatal("expected error for closed server")
	}
}

// newTestClient creates a Client pointing at a test server instead of the real API.
func newTestClient(apiKey, baseURL string) *Client {
	c := NewClient(apiKey)
	c.baseURL = baseURL
	return c
}
