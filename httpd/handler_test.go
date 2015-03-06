package httpd_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/influxdb/influxdb"
	"github.com/influxdb/influxdb/client"
	"github.com/influxdb/influxdb/httpd"
	"github.com/influxdb/influxdb/influxql"
	"github.com/influxdb/influxdb/messaging"
)

func init() {
	influxdb.BcryptCost = 4
}

func TestBatchWrite_UnmarshalEpoch(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name      string
		epoch     int64
		precision string
		expected  time.Time
	}{
		{
			name:      "nanoseconds",
			epoch:     now.UnixNano(),
			precision: "n",
			expected:  now,
		},
		{
			name:      "microseconds",
			epoch:     now.Round(time.Microsecond).UnixNano() / int64(time.Microsecond),
			precision: "u",
			expected:  now.Round(time.Microsecond),
		},
		{
			name:      "milliseconds",
			epoch:     now.Round(time.Millisecond).UnixNano() / int64(time.Millisecond),
			precision: "ms",
			expected:  now.Round(time.Millisecond),
		},
		{
			name:      "seconds",
			epoch:     now.Round(time.Second).UnixNano() / int64(time.Second),
			precision: "s",
			expected:  now.Round(time.Second),
		},
		{
			name:      "minutes",
			epoch:     now.Round(time.Minute).UnixNano() / int64(time.Minute),
			precision: "m",
			expected:  now.Round(time.Minute),
		},
		{
			name:      "hours",
			epoch:     now.Round(time.Hour).UnixNano() / int64(time.Hour),
			precision: "h",
			expected:  now.Round(time.Hour),
		},
		{
			name:      "max int64",
			epoch:     9223372036854775807,
			precision: "n",
			expected:  time.Unix(0, 9223372036854775807),
		},
		{
			name:      "100 years from now",
			epoch:     now.Add(time.Hour * 24 * 365 * 100).UnixNano(),
			precision: "n",
			expected:  now.Add(time.Hour * 24 * 365 * 100),
		},
	}

	for _, test := range tests {
		t.Logf("testing %q\n", test.name)
		data := []byte(fmt.Sprintf(`{"timestamp": %d, "precision":"%s"}`, test.epoch, test.precision))
		t.Logf("json: %s", string(data))
		var bp client.BatchPoints
		err := json.Unmarshal(data, &bp)
		if err != nil {
			t.Fatalf("unexpected error.  expected: %v, actual: %v", nil, err)
		}
		if !bp.Timestamp.Equal(test.expected) {
			t.Fatalf("Unexpected time.  expected: %v, actual: %v", test.expected, bp.Timestamp)
		}
	}
}

func TestBatchWrite_UnmarshalRFC(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		rfc      string
		now      time.Time
		expected time.Time
	}{
		{
			name:     "RFC3339Nano",
			rfc:      time.RFC3339Nano,
			now:      now,
			expected: now,
		},
		{
			name:     "RFC3339",
			rfc:      time.RFC3339,
			now:      now.Round(time.Second),
			expected: now.Round(time.Second),
		},
	}

	for _, test := range tests {
		t.Logf("testing %q\n", test.name)
		ts := test.now.Format(test.rfc)
		data := []byte(fmt.Sprintf(`{"timestamp": %q}`, ts))
		t.Logf("json: %s", string(data))
		var bp client.BatchPoints
		err := json.Unmarshal(data, &bp)
		if err != nil {
			t.Fatalf("unexpected error.  exptected: %v, actual: %v", nil, err)
		}
		if !bp.Timestamp.Equal(test.expected) {
			t.Fatalf("Unexpected time.  expected: %v, actual: %v", test.expected, bp.Timestamp)
		}
	}
}

func TestHandler_Databases(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateDatabase("bar")
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "SHOW DATABASES"}, nil, "")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{"series":[{"columns":["name"],"values":[["bar"],["foo"]]}]}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_DatabasesPrettyPrinted(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateDatabase("bar")
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "SHOW DATABASES", "pretty": "true"}, nil, "")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{
    "results": [
        {
            "series": [
                {
                    "columns": [
                        "name"
                    ],
                    "values": [
                        [
                            "bar"
                        ],
                        [
                            "foo"
                        ]
                    ]
                }
            ]
        }
    ]
}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_CreateDatabase(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "CREATE DATABASE foo"}, nil, "")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_CreateDatabase_BadRequest_NoName(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "CREATE DATABASE"}, nil, "")
	if status != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_CreateDatabase_Conflict(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "CREATE DATABASE foo"}, nil, "")
	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{"error":"database exists"}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_DropDatabase(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "DROP DATABASE foo"}, nil, "")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_DropDatabase_NotFound(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "DROP DATABASE bar"}, nil, "")
	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{"error":"database not found"}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_RetentionPolicies(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "SHOW RETENTION POLICIES foo"}, nil, "")

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{"series":[{"columns":["name","duration","replicaN"],"values":[["bar","168h0m0s",1]]}]}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_RetentionPolicies_DatabaseNotFound(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "SHOW RETENTION POLICIES foo"}, nil, "")

	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{"error":"database not found"}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_CreateRetentionPolicy(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "CREATE RETENTION POLICY bar ON foo DURATION 1h REPLICATION 1"}
	status, body := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_CreateRetentionPolicyAsDefault(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "CREATE RETENTION POLICY bar ON foo DURATION 1h REPLICATION 1 DEFAULT"}
	status, body := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{}]}` {
		t.Fatalf("unexpected body: %s", body)
	}

	rp, err := srvr.DefaultRetentionPolicy("foo")
	if err != nil {
		t.Fatal(err)
	} else if rp.Name != "bar" {
		t.Fatalf("default retention policy mismatch:\n  exp=%s\n  got=%s\n", "bar", rp.Name)
	}
}

func TestHandler_CreateRetentionPolicy_DatabaseNotFound(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "CREATE RETENTION POLICY bar ON foo DURATION 1h REPLICATION 1"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_CreateRetentionPolicy_Conflict(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "CREATE RETENTION POLICY bar ON foo DURATION 1h REPLICATION 1"}
	MustHTTP("GET", s.URL+`/query`, query, nil, "")

	status, _ := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_CreateRetentionPolicy_BadRequest(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "CREATE RETENTION POLICY bar ON foo DURATION ***BAD*** REPLICATION 1"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_UpdateRetentionPolicy(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "ALTER RETENTION POLICY bar ON foo REPLICATION 42 DURATION 1m DEFAULT"}
	status, body := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	// Verify updated policy.
	p, _ := srvr.RetentionPolicy("foo", "bar")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{}]}` {
		t.Fatalf("unexpected body: %s", body)
	} else if p.ReplicaN != 42 {
		t.Fatalf("unexpected replication factor: %d", p.ReplicaN)
	}

	// Make sure retention policy has been set as default.
	if p, err := srvr.DefaultRetentionPolicy("foo"); err != nil {
		t.Fatal(err)
	} else if p == nil {
		t.Fatal("default retention policy not set")
	} else if p.Name != "bar" {
		t.Fatal(`expected default retention policy to be "bar"`)
	}
}

func TestHandler_UpdateRetentionPolicy_BadRequest(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "ALTER RETENTION POLICY bar ON foo DURATION ***BAD*** REPLICATION 1"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	// Verify response.
	if status != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_UpdateRetentionPolicy_DatabaseNotFound(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "ALTER RETENTION POLICY bar ON foo DURATION 1h REPLICATION 1"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	// Verify response.
	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_UpdateRetentionPolicy_NotFound(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "ALTER RETENTION POLICY qux ON foo DURATION 1h REPLICATION 1"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	// Verify response.
	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_DeleteRetentionPolicy(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "DROP RETENTION POLICY bar ON foo"}
	status, body := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_DeleteRetentionPolicy_DatabaseNotFound(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "DROP RETENTION POLICY bar ON qux"}
	status, body := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{"error":"database not found"}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_DeleteRetentionPolicy_NotFound(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "DROP RETENTION POLICY bar ON foo"}
	status, body := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{"error":"retention policy not found"}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_GzipEnabled(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	req, err := http.NewRequest("GET", s.URL+`/ping`, bytes.NewBuffer([]byte{}))
	if err != nil {
		panic(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}

	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("unexpected Content-Encoding.  expected %q, actual: %q", "gzip", ce)
	}
}

func TestHandler_GzipDisabled(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	req, err := http.NewRequest("GET", s.URL+`/ping`, bytes.NewBuffer([]byte{}))
	if err != nil {
		panic(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}

	if ce := resp.Header.Get("Content-Encoding"); ce == "gzip" {
		t.Fatalf("unexpected Content-Encoding.  expected %q, actual: %q", "", ce)
	}
}

func TestHandler_Index(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL, nil, nil, "")

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}

	if body != "1" {
		t.Fatalf("unexpected body.  expected %q, actual %q", "1", body)
	}
}

func TestHandler_Wait(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/wait/1`, map[string]string{"timeout": "1"}, nil, "")

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}

	if body != "1" {
		t.Fatalf("unexpected body.  expected %q, actual %q", "1", body)
	}
}

func TestHandler_WaitIncrement(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))

	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("GET", s.URL+`/wait/2`, map[string]string{"timeout": "200"}, nil, "")

	// Write some data
	_, _ = MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"timestamp": "2009-11-10T23:00:00Z","fields": {"value": 100}}]}`)

	if status != http.StatusOK {
		t.Fatalf("unexpected status, expected:  %d, actual: %d", http.StatusOK, status)
	}
}

func TestHandler_WaitNoIndexSpecified(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("GET", s.URL+`/wait`, nil, nil, "")

	if status != http.StatusNotFound {
		t.Fatalf("unexpected status, expected:  %d, actual: %d", http.StatusNotFound, status)
	}
}

func TestHandler_WaitInvalidIndexSpecified(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("GET", s.URL+`/wait/foo`, nil, nil, "")

	if status != http.StatusBadRequest {
		t.Fatalf("unexpected status, expected:  %d, actual: %d", http.StatusBadRequest, status)
	}
}

func TestHandler_WaitExpectTimeout(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("GET", s.URL+`/wait/2`, map[string]string{"timeout": "1"}, nil, "")

	if status != http.StatusRequestTimeout {
		t.Fatalf("unexpected status, expected:  %d, actual: %d", http.StatusRequestTimeout, status)
	}
}

func TestHandler_Ping(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("GET", s.URL+`/ping`, nil, nil, "")

	if status != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_PingHead(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("HEAD", s.URL+`/ping`, nil, nil, "")

	if status != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_Users_MultipleUsers(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateUser("jdoe", "1337", false)
	srvr.CreateUser("mclark", "1337", true)
	srvr.CreateUser("csmith", "1337", false)
	s := NewHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "SHOW USERS"}
	status, body := MustHTTP("GET", s.URL+`/query`, query, nil, "")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"results":[{"series":[{"columns":["user","admin"],"values":[["csmith",false],["jdoe",false],["mclark",true]]}]}]}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_UpdateUser(t *testing.T) {
	t.Skip()
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateUser("jdoe", "1337", false)
	s := NewHTTPServer(srvr)
	defer s.Close()

	// Save original password hash.
	hash := srvr.User("jdoe").Hash

	// Update user password.
	status, body := MustHTTP("PUT", s.URL+`/users/jdoe`, nil, nil, `{"password": "7331"}`)
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `` {
		t.Fatalf("unexpected body: %s", body)
	} else if srvr.User("jdoe").Hash == hash {
		t.Fatalf("expected password hash to change")
	}
}

func TestHandler_UpdateUser_PasswordBadRequest(t *testing.T) {
	t.Skip()
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateUser("jdoe", "1337", false)
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("PUT", s.URL+`/users/jdoe`, nil, nil, `{"password": 10}`)
	if status != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `json: cannot unmarshal number into Go value of type string` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_DataNodes(t *testing.T) {
	t.Skip()
	srvr := OpenUninitializedServer(NewMessagingClient())
	srvr.CreateDataNode(MustParseURL("http://localhost:1000"))
	srvr.CreateDataNode(MustParseURL("http://localhost:2000"))
	srvr.CreateDataNode(MustParseURL("http://localhost:3000"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("GET", s.URL+`/data_nodes`, nil, nil, "")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `[{"id":1,"url":"http://localhost:1000"},{"id":2,"url":"http://localhost:2000"},{"id":3,"url":"http://localhost:3000"}]` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_CreateDataNode(t *testing.T) {
	t.Skip()
	srvr := OpenUninitializedServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("POST", s.URL+`/data_nodes`, nil, nil, `{"url":"http://localhost:1000"}`)
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `{"id":1,"url":"http://localhost:1000"}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_CreateDataNode_BadRequest(t *testing.T) {
	t.Skip()
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("POST", s.URL+`/data_nodes`, nil, nil, `{"name":`)
	if status != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `unexpected EOF` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_CreateDataNode_InternalServerError(t *testing.T) {
	t.Skip()
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("POST", s.URL+`/data_nodes`, nil, nil, `{"url":""}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d, %s", status, body)
	} else if body != `data node url required` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_DeleteDataNode(t *testing.T) {
	t.Skip()
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDataNode(MustParseURL("http://localhost:1000"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("DELETE", s.URL+`/data_nodes/1`, nil, nil, "")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandler_DeleteUser_DataNodeNotFound(t *testing.T) {
	t.Skip()
	srvr := OpenAuthlessServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("DELETE", s.URL+`/data_nodes/10000`, nil, nil, "")
	if status != http.StatusNotFound {
		t.Fatalf("unexpected status: %d", status)
	} else if body != `data node not found` {
		t.Fatalf("unexpected body: %s", body)
	}
}

// Perform a subset of endpoint testing, with authentication enabled.

func TestHandler_AuthenticatedCreateAdminUser(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	// Attempting to create a non-admin user should fail.
	query := map[string]string{"q": "CREATE USER maeve WITH PASSWORD 'pass'"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", status)
	}

	// Creating an admin user, without supplying authentication credentials should fail.
	query = map[string]string{"q": "CREATE USER louise WITH PASSWORD 'pass' WITH ALL PRIVILEGES"}
	status, _ = MustHTTP("GET", s.URL+`/query`, query, nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", status)
	}

}

func TestHandler_AuthenticatedDatabases_Unauthorized(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("GET", s.URL+`/query`, map[string]string{"q": "SHOW DATABASES"}, nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_AuthenticatedDatabases_AuthorizedQueryParams(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	srvr.CreateUser("lisa", "password", true)
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "SHOW DATABASES", "u": "lisa", "p": "password"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, nil, "")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_AuthenticatedDatabases_UnauthorizedQueryParams(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	srvr.CreateUser("lisa", "password", true)
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	query := map[string]string{"q": "SHOW DATABASES", "u": "lisa", "p": "wrong"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_AuthenticatedDatabases_AuthorizedBasicAuth(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	srvr.CreateUser("lisa", "password", true)
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	auth := make(map[string]string)
	auth["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("lisa:password"))
	query := map[string]string{"q": "SHOW DATABASES"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, auth, "")
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_AuthenticatedDatabases_UnauthorizedBasicAuth(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	srvr.CreateUser("lisa", "password", true)
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	auth := make(map[string]string)
	auth["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("lisa:wrong"))
	query := map[string]string{"q": "SHOW DATABASES"}
	status, _ := MustHTTP("GET", s.URL+`/query`, query, auth, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_GrantDBPrivilege(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	// Create a cluster admin that will grant privilege to "john".
	srvr.CreateUser("lisa", "password", true)
	// Create user that will be granted a privilege.
	srvr.CreateUser("john", "password", false)
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	auth := make(map[string]string)
	auth["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("lisa:password"))
	query := map[string]string{"q": "GRANT READ ON foo TO john"}

	status, _ := MustHTTP("GET", s.URL+`/query`, query, auth, "")

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}

	u := srvr.User("john")
	if p, ok := u.Privileges["foo"]; !ok {
		t.Fatal(`expected john to have privileges on foo but he has none`)
	} else if p != influxql.ReadPrivilege {
		t.Fatalf(`expected john to have read privilege on foo but he has %s`, p.String())
	}

	// Make sure update persists after server restart.
	srvr.Restart()

	u = srvr.User("john")
	if p, ok := u.Privileges["foo"]; !ok {
		t.Fatal(`expected john to have privileges on foo but he has none after restart`)
	} else if p != influxql.ReadPrivilege {
		t.Fatalf(`expected john to have read privilege on foo but he has %s after restart`, p.String())
	}
}

func TestHandler_RevokeAdmin(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	// Create a cluster admin that will revoke admin from "john".
	srvr.CreateUser("lisa", "password", true)
	// Create user that will have cluster admin revoked.
	srvr.CreateUser("john", "password", true)
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	auth := make(map[string]string)
	auth["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("lisa:password"))
	query := map[string]string{"q": "REVOKE ALL PRIVILEGES FROM john"}

	status, body := MustHTTP("GET", s.URL+`/query`, query, auth, "")

	if status != http.StatusOK {
		t.Log(body)
		t.Fatalf("unexpected status: %d", status)
	}

	if u := srvr.User("john"); u.Admin {
		t.Fatal(`expected user "john" not to be admin`)
	}

	// Make sure update persists after server restart.
	srvr.Restart()

	if u := srvr.User("john"); u.Admin {
		t.Fatal(`expected user "john" not to be admin after restart`)
	}
}

func TestHandler_RevokeDBPrivilege(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	// Create a cluster admin that will revoke privilege from "john".
	srvr.CreateUser("lisa", "password", true)
	// Create user that will have privilege revoked.
	srvr.CreateUser("john", "password", false)
	u := srvr.User("john")
	u.Privileges["foo"] = influxql.ReadPrivilege
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	auth := make(map[string]string)
	auth["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("lisa:password"))
	query := map[string]string{"q": "REVOKE READ ON foo FROM john"}

	status, body := MustHTTP("GET", s.URL+`/query`, query, auth, "")

	if status != http.StatusOK {
		t.Log(body)
		t.Fatalf("unexpected status: %d", status)
	}

	if p := u.Privileges["foo"]; p != influxql.NoPrivileges {
		t.Fatal(`expected user "john" not to have privileges on foo`)
	}

	// Make sure update persists after server restart.
	srvr.Restart()

	if p := u.Privileges["foo"]; p != influxql.NoPrivileges {
		t.Fatal(`expected user "john" not to have privileges on foo after restart`)
	}
}

func TestHandler_DropSeries(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"timestamp": "2009-11-10T23:00:00Z","fields": {"value": 100}}]}`)

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}

	query := map[string]string{"db": "foo", "q": "DROP SERIES FROM cpu"}
	status, _ = MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_serveWriteSeries(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"timestamp": "2009-11-10T23:00:00Z","fields": {"value": 100}}]}`)

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}
}

func TestHandler_serveWriteSeriesWithNoFields(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"timestamp": "2009-11-10T23:00:00Z"}]}`)

	expected := fmt.Sprintf(`{"error":"%s"}`, influxdb.ErrFieldsRequired.Error())

	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", status)
	} else if body != expected {
		t.Fatalf("result mismatch:\n\texp=%s\n\tgot=%s\n", expected, body)
	}
}

func TestHandler_serveWriteSeriesWithAuthNilUser(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"timestamp": "2009-11-10T23:00:00Z","fields": {"value": 100}}]}`)

	if status != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", status)
	}

	response := `{"error":"user is required to write to database \"foo\""}`
	if body != response {
		t.Fatalf("unexpected body: expected %s, actual %s", response, body)
	}
}

func TestHandler_serveWriteSeries_noDatabaseExists(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"timestamp": "2009-11-10T23:00:00Z","fields": {"value": 100}}]}`)

	expectedStatus := http.StatusNotFound
	if status != expectedStatus {
		t.Fatalf("unexpected status: expected: %d, actual: %d", expectedStatus, status)
	}

	response := `{"error":"database not found: \"foo\""}`
	if body != response {
		t.Fatalf("unexpected body: expected %s, actual %s", response, body)
	}
}

func TestHandler_serveWriteSeries_invalidJSON(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"timestamp": "2009-11-10T23:00:00Z","fields": {"value": 100}}]}`)

	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: expected: %d, actual: %d", http.StatusInternalServerError, status)
	}

	response := `{"error":"invalid character 'o' in literal false (expecting 'a')"}`
	if body != response {
		t.Fatalf("unexpected body: expected %s, actual %s", response, body)
	}
}

func TestHandler_serveWriteSeries_noDatabaseSpecified(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	s := NewHTTPServer(srvr)
	defer s.Close()

	status, body := MustHTTP("POST", s.URL+`/write`, nil, nil, `{}`)

	if status != http.StatusInternalServerError {
		t.Fatalf("unexpected status: expected: %d, actual: %d", http.StatusInternalServerError, status)
	}

	response := `{"error":"database is required"}`
	if body != response {
		t.Fatalf("unexpected body: expected %s, actual %s", response, body)
	}
}

func TestHandler_serveWriteSeriesNonZeroTime(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	srvr.SetDefaultRetentionPolicy("foo", "bar")

	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"timestamp": "2009-11-10T23:00:00Z", "fields": {"value": 100}}]}`)
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}
	time.Sleep(100 * time.Millisecond) // Ensure data node picks up write.

	srvr.Restart() // Ensure data is queryable across restarts.

	query := map[string]string{"db": "foo", "q": "select value from cpu"}
	status, body := MustHTTP("GET", s.URL+`/query`, query, nil, "")
	if status != http.StatusOK {
		t.Logf("query %s\n", query)
		t.Log(body)
		t.Errorf("unexpected status: %d", status)
	}

	r := &influxdb.Results{}
	if err := json.Unmarshal([]byte(body), r); err != nil {
		t.Logf("query : %s\n", query)
		t.Log(body)
		t.Error(err)
	}
	if len(r.Results) != 1 {
		t.Fatalf("unexpected results count")
	}

	result := r.Results[0]
	if len(result.Series) != 1 {
		t.Fatalf("unexpected row count, expected: %d, actual: %d", 1, len(result.Series))
	}
}

func TestHandler_serveWriteSeriesZeroTime(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	srvr.SetDefaultRetentionPolicy("foo", "bar")

	s := NewHTTPServer(srvr)
	defer s.Close()

	now := time.Now()

	status, _ := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"fields": {"value": 100}}]}`)

	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}
	time.Sleep(100 * time.Millisecond) // Ensure data node picks up write.

	srvr.Restart() // Ensure data is queryable across restarts.

	query := map[string]string{"db": "foo", "q": "select value from cpu"}
	status, body := MustHTTP("GET", s.URL+`/query`, query, nil, "")

	if status != http.StatusOK {
		t.Logf("query %s\n", query)
		t.Log(body)
		t.Errorf("unexpected status: %d", status)
	}

	r := &influxdb.Results{}
	if err := json.Unmarshal([]byte(body), r); err != nil {
		t.Logf("query : %s\n", query)
		t.Log(body)
		t.Error(err)
	}

	if len(r.Results) != 1 {
		t.Fatalf("unexpected results count")
	}
	result := r.Results[0]
	if len(result.Series) != 1 {
		t.Fatalf("unexpected row count, expected: %d, actual: %d", 1, len(result.Series))
	}
	row := result.Series[0]
	timestamp, _ := time.Parse(time.RFC3339Nano, row.Values[0][0].(string))
	if timestamp.IsZero() {
		t.Fatalf("failed to write a default time, actual: %v", row.Values[0][0])
	}
	if !timestamp.After(now) {
		t.Fatalf("time was not valid. expected something after %v, actual: %v", now, timestamp)
	}
}

func TestHandler_serveWriteSeriesBatch(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	srvr.SetDefaultRetentionPolicy("foo", "bar")

	s := NewHTTPServer(srvr)
	defer s.Close()

	batch := `
{
    "database": "foo",
    "retentionPolicy": "bar",
    "points": [
        {
            "name": "disk",
            "timestamp": "2009-11-10T23:00:00Z",
            "tags": {
                "host": "server01"
            },
            "fields": {
                "full": false
            }
        },
        {
            "name": "disk",
            "timestamp": "2009-11-10T23:00:01Z",
            "tags": {
                "host": "server01"
            },
            "fields": {
                "full": true
            }
        },
        {
            "name": "disk",
            "timestamp": "2009-11-10T23:00:02Z",
            "tags": {
                "host": "server02"
            },
            "fields": {
                "full_pct": 64
            }
        }
    ]
}
`
	status, body := MustHTTP("POST", s.URL+`/write`, nil, nil, batch)
	if status != http.StatusOK {
		t.Log(body)
		t.Fatalf("unexpected status: %d", status)
	}
	time.Sleep(200 * time.Millisecond) // Ensure data node picks up write.

	query := map[string]string{"db": "foo", "q": "select * from disk"}
	status, body = MustHTTP("GET", s.URL+`/query`, query, nil, "")
	if status != http.StatusOK {
		t.Logf("query %s\n", query)
		t.Log(body)
		t.Errorf("unexpected status: %d", status)
	}

	r := &influxdb.Results{}
	if err := json.Unmarshal([]byte(body), r); err != nil {
		t.Logf("query : %s\n", query)
		t.Log(body)
		t.Error(err)
	}
	if len(r.Results) != 1 {
		t.Fatalf("unexpected results count")
	}
	result := r.Results[0]
	if len(result.Series) != 1 {
		t.Fatalf("unexpected row count, expected: %d, actual: %d", 1, len(result.Series))
	}
	if len(result.Series[0].Columns) != 3 {
		t.Fatalf("unexpected column count, expected: %d, actual: %d", 3, len(result.Series[0].Columns))
	}
	if len(result.Series[0].Values) != 3 {
		t.Fatalf("unexpected values count, expected: %d, actual: %d", 3, len(result.Series[0].Values))
	}
}

func TestHandler_serveWriteSeriesFieldTypeConflict(t *testing.T) {
	srvr := OpenAuthlessServer(NewMessagingClient())
	srvr.CreateDatabase("foo")
	srvr.CreateRetentionPolicy("foo", influxdb.NewRetentionPolicy("bar"))
	srvr.SetDefaultRetentionPolicy("foo", "bar")

	s := NewHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"fields": {"value": 100}}]}`)
	if status != http.StatusOK {
		t.Fatalf("unexpected status: %d", status)
	}

	status, body := MustHTTP("POST", s.URL+`/write`, nil, nil, `{"database" : "foo", "retentionPolicy" : "bar", "points": [{"name": "cpu", "tags": {"host": "server01"},"fields": {"value": "foo"}}]}`)
	if status != http.StatusInternalServerError {
		t.Errorf("unexpected status: %d", status)
	}

	r := &influxdb.Results{}
	if err := json.Unmarshal([]byte(body), r); err != nil {
		t.Log(body)
		t.Error(err)
	}
	if len(r.Results) != 0 {
		t.Fatalf("unexpected results count")
	}
	if r.Err.Error() != "field \"value\" is type string, mapped as type number" {
		t.Fatalf("unexpected error returned, actual: %s", r.Err.Error())
	}
}

// str2iface converts an array of strings to an array of interfaces.
func str2iface(strs []string) []interface{} {
	a := make([]interface{}, 0, len(strs))
	for _, s := range strs {
		a = append(a, interface{}(s))
	}
	return a
}

func TestHandler_ProcessContinousQueries(t *testing.T) {
	srvr := OpenAuthenticatedServer(NewMessagingClient())
	s := NewAuthenticatedHTTPServer(srvr)
	defer s.Close()

	status, _ := MustHTTP("POST", s.URL+`/process_continuous_queries`, nil, nil, "")
	if status != http.StatusAccepted {
		t.Fatalf("unexpected status: %d", status)
	}
}

// batchWrite JSON Unmarshal tests

// Utility functions for this test suite.

func MustHTTP(verb, path string, params, headers map[string]string, body string) (int, string) {
	req, err := http.NewRequest(verb, path, bytes.NewBuffer([]byte(body)))
	if err != nil {
		panic(err)
	}

	if params != nil {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimRight(string(b), "\n")
}

// MustParseURL parses a string into a URL. Panic on error.
func MustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err.Error())
	}
	return u
}

// Server is a test HTTP server that wraps a handler
type HTTPServer struct {
	*httptest.Server
	Handler *httpd.Handler
}

func NewHTTPServer(s *Server) *HTTPServer {
	h := httpd.NewHandler(s.Server, false, "X.X")
	return &HTTPServer{httptest.NewServer(h), h}
}

func NewAuthenticatedHTTPServer(s *Server) *HTTPServer {
	h := httpd.NewHandler(s.Server, true, "X.X")
	return &HTTPServer{httptest.NewServer(h), h}
}

func (s *HTTPServer) Close() {
	s.Server.Close()
}

// Server is a wrapping test struct for influxdb.Server.
type Server struct {
	*influxdb.Server
}

// NewServer returns a new test server instance.
func NewServer() *Server {
	return &Server{influxdb.NewServer()}
}

// OpenAuthenticatedServer returns a new, open test server instance with authentication enabled.
func OpenAuthenticatedServer(client influxdb.MessagingClient) *Server {
	s := OpenUninitializedServer(client)
	if err := s.Initialize(&url.URL{Host: "127.0.0.1:8080"}); err != nil {
		panic(err.Error())
	}
	return s
}

// OpenAuthlessServer returns a new, open test server instance without authentication enabled.
func OpenAuthlessServer(client influxdb.MessagingClient) *Server {
	s := OpenAuthenticatedServer(client)
	s.SetAuthenticationEnabled(false)
	return s
}

// Close shuts down the server and removes all temporary files.
func (s *Server) Close() {
	defer os.RemoveAll(s.Path())
	s.Server.Close()
}

// Restart stops and restarts the server.
func (s *Server) Restart() {
	path, client := s.Path(), s.Client()

	// Stop the server.
	if err := s.Server.Close(); err != nil {
		panic("close: " + err.Error())
	}

	// Open and reset the client.
	if err := s.Server.Open(path); err != nil {
		panic("open: " + err.Error())
	}
	if err := s.Server.SetClient(client); err != nil {
		panic("client: " + err.Error())
	}
}

// OpenUninitializedServer returns a new, uninitialized, open test server instance.
func OpenUninitializedServer(client influxdb.MessagingClient) *Server {
	s := NewServer()
	if err := s.Open(tempfile()); err != nil {
		panic(err.Error())
	}
	if err := s.SetClient(client); err != nil {
		panic(err.Error())
	}
	return s
}

// TODO corylanou: evaluate how much of this should be in this package
// vs. how much should be a mocked out interface
// MessagingClient represents a test client for the messaging broker.
type MessagingClient struct {
	index uint64
	c     chan *messaging.Message
	mu    sync.Mutex // Ensure all publishing is serialized.

	PublishFunc       func(*messaging.Message) (uint64, error)
	CreateReplicaFunc func(replicaID uint64, connectURL *url.URL) error
	DeleteReplicaFunc func(replicaID uint64) error
	SubscribeFunc     func(replicaID, topicID uint64) error
	UnsubscribeFunc   func(replicaID, topicID uint64) error
}

// NewMessagingClient returns a new instance of MessagingClient.
func NewMessagingClient() *MessagingClient {
	c := &MessagingClient{c: make(chan *messaging.Message, 1)}
	c.PublishFunc = c.send
	c.CreateReplicaFunc = func(replicaID uint64, connectURL *url.URL) error { return nil }
	c.DeleteReplicaFunc = func(replicaID uint64) error { return nil }
	c.SubscribeFunc = func(replicaID, topicID uint64) error { return nil }
	c.UnsubscribeFunc = func(replicaID, topicID uint64) error { return nil }
	return c
}

// Publish attaches an autoincrementing index to the message.
// This function also execute's the client's PublishFunc mock function.
func (c *MessagingClient) Publish(m *messaging.Message) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index++
	m.Index = c.index
	return c.PublishFunc(m)
}

// send sends the message through to the channel.
// This is the default value of PublishFunc.
func (c *MessagingClient) send(m *messaging.Message) (uint64, error) {
	c.c <- m
	return m.Index, nil
}

// Creates a new replica with a given ID on the broker.
func (c *MessagingClient) CreateReplica(replicaID uint64, connectURL *url.URL) error {
	return c.CreateReplicaFunc(replicaID, connectURL)
}

// Deletes an existing replica with a given ID from the broker.
func (c *MessagingClient) DeleteReplica(replicaID uint64) error {
	return c.DeleteReplicaFunc(replicaID)
}

// Subscribe adds a subscription to a replica for a topic on the broker.
func (c *MessagingClient) Subscribe(replicaID, topicID uint64) error {
	return c.SubscribeFunc(replicaID, topicID)
}

// Unsubscribe removes a subscrition from a replica for a topic on the broker.
func (c *MessagingClient) Unsubscribe(replicaID, topicID uint64) error {
	return c.UnsubscribeFunc(replicaID, topicID)
}

// C returns a channel for streaming message.
func (c *MessagingClient) C() <-chan *messaging.Message { return c.c }

// tempfile returns a temporary path.
func tempfile() string {
	f, _ := ioutil.TempFile("", "influxdb-")
	path := f.Name()
	f.Close()
	os.Remove(path)
	return path
}

// errstring converts an error to its string representation.
func errstring(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

// marshalJSON marshals input to a string of JSON or panics on error.
func mustMarshalJSON(i interface{}) string {
	b, err := json.Marshal(i)
	if err != nil {
		panic(err)
	}
	return string(b)
}
