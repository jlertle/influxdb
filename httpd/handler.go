package httpd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"compress/gzip"

	"code.google.com/p/go-uuid/uuid"

	"github.com/bmizerany/pat"
	"github.com/influxdb/influxdb"
	"github.com/influxdb/influxdb/client"
	"github.com/influxdb/influxdb/influxql"
)

// TODO: Standard response headers (see: HeaderHandler)
// TODO: Compression (see: CompressionHeaderHandler)

// TODO: Check HTTP response codes: 400, 401, 403, 409.

type route struct {
	name        string
	method      string
	pattern     string
	gzipped     bool
	log         bool
	handlerFunc interface{}
}

// Handler represents an HTTP handler for the InfluxDB server.
type Handler struct {
	server                *influxdb.Server
	routes                []route
	mux                   *pat.PatternServeMux
	requireAuthentication bool

	Logger     *log.Logger
	WriteTrace bool // Detailed logging of write path
}

// NewHandler returns a new instance of Handler.
func NewHandler(s *influxdb.Server, requireAuthentication bool, version string) *Handler {
	h := &Handler{
		server: s,
		mux:    pat.New(),
		requireAuthentication: requireAuthentication,
		Logger:                log.New(os.Stderr, "[http] ", log.LstdFlags),
	}

	h.routes = append(h.routes,
		route{
			"query", // Query serving route.
			"GET", "/query", true, true, h.serveQuery,
		},
		route{
			"write", // Data-ingest route.
			"OPTIONS", "/write", true, true, h.serveOptions,
		},
		route{
			"write", // Data-ingest route.
			"POST", "/write", true, true, h.serveWrite,
		},
		route{ // List data nodes
			"data_nodes_index",
			"GET", "/data_nodes", true, false, h.serveDataNodes,
		},
		route{ // Create data node
			"data_nodes_create",
			"POST", "/data_nodes", true, false, h.serveCreateDataNode,
		},
		route{ // Delete data node
			"data_nodes_delete",
			"DELETE", "/data_nodes/:id", true, false, h.serveDeleteDataNode,
		},
		route{ // Metastore
			"metastore",
			"GET", "/metastore", false, false, h.serveMetastore,
		},
		route{ // Status
			"status",
			"GET", "/status", true, true, h.serveStatus,
		},
		route{ // Ping
			"ping",
			"GET", "/ping", true, true, h.servePing,
		},
		route{ // Ping
			"ping-head",
			"HEAD", "/ping", true, true, h.servePing,
		},
		route{ // Tell data node to run CQs that should be run
			"process_continuous_queries",
			"POST", "/process_continuous_queries", false, false, h.serveProcessContinuousQueries,
		},
		route{
			"wait", // Wait.
			"GET", "/wait/:index", true, true, h.serveWait,
		},
		route{
			"index", // Index.
			"GET", "/", true, true, h.serveIndex,
		},
	)

	for _, r := range h.routes {
		var handler http.Handler

		// If it's a handler func that requires authorization, wrap it in authorization
		if hf, ok := r.handlerFunc.(func(http.ResponseWriter, *http.Request, *influxdb.User)); ok {
			handler = authenticate(hf, h, requireAuthentication)
		}
		// This is a normal handler signature and does not require authorization
		if hf, ok := r.handlerFunc.(func(http.ResponseWriter, *http.Request)); ok {
			handler = http.HandlerFunc(hf)
		}

		if r.gzipped {
			handler = gzipFilter(handler)
		}
		handler = versionHeader(handler, version)
		handler = cors(handler)
		handler = requestID(handler)
		if r.log {
			handler = logging(handler, r.name, h.Logger)
		}
		handler = recovery(handler, r.name, h.Logger) // make sure recovery is always last

		h.mux.Add(r.method, r.pattern, handler)
	}

	return h
}

// SetLogOutput sets writer for all handler log output.
func (h *Handler) SetLogOutput(w io.Writer) {
	h.Logger = log.New(w, "[http] ", log.LstdFlags)
}

// ServeHTTP responds to HTTP request to the handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// serveQuery parses an incoming query and, if valid, executes the query.
func (h *Handler) serveQuery(w http.ResponseWriter, r *http.Request, user *influxdb.User) {
	q := r.URL.Query()
	p := influxql.NewParser(strings.NewReader(q.Get("q")))
	db := q.Get("db")
	pretty := q.Get("pretty") == "true"

	// Parse query from query string.
	query, err := p.ParseQuery()
	if err != nil {
		httpError(w, "error parsing query: "+err.Error(), pretty, http.StatusBadRequest)
		return
	}

	// Execute query. One result will return for each statement.
	results := h.server.ExecuteQuery(query, db, user)

	// Send results to client.
	httpResults(w, results, pretty)
}

// serveWrite receives incoming series data and writes it to the database.
func (h *Handler) serveWrite(w http.ResponseWriter, r *http.Request, user *influxdb.User) {
	var bp client.BatchPoints
	var dec *json.Decoder

	if h.WriteTrace {
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			h.Logger.Print("write handler failed to read bytes from request body")
		} else {
			h.Logger.Printf("write body received by handler: %s", string(b))
		}
		dec = json.NewDecoder(strings.NewReader(string(b)))
	} else {
		dec = json.NewDecoder(r.Body)
		defer r.Body.Close()
	}

	var writeError = func(result influxdb.Result, statusCode int) {
		w.WriteHeader(statusCode)
		w.Header().Add("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(&result)
		return
	}

	if err := dec.Decode(&bp); err != nil {
		if err.Error() == "EOF" {
			w.WriteHeader(http.StatusOK)
			return
		}
		writeError(influxdb.Result{Err: err}, http.StatusInternalServerError)
		return
	}

	if bp.Database == "" {
		writeError(influxdb.Result{Err: fmt.Errorf("database is required")}, http.StatusInternalServerError)
		return
	}

	if !h.server.DatabaseExists(bp.Database) {
		writeError(influxdb.Result{Err: fmt.Errorf("database not found: %q", bp.Database)}, http.StatusNotFound)
		return
	}

	if h.requireAuthentication && user == nil {
		writeError(influxdb.Result{Err: fmt.Errorf("user is required to write to database %q", bp.Database)}, http.StatusUnauthorized)
		return
	}

	if h.requireAuthentication && !user.Authorize(influxql.WritePrivilege, bp.Database) {
		writeError(influxdb.Result{Err: fmt.Errorf("%q user is not authorized to write to database %q", user.Name, bp.Database)}, http.StatusUnauthorized)
		return
	}

	points, err := influxdb.NormalizeBatchPoints(bp)
	if err != nil {
		writeError(influxdb.Result{Err: err}, http.StatusInternalServerError)
		return
	}

	if index, err := h.server.WriteSeries(bp.Database, bp.RetentionPolicy, points); err != nil {
		writeError(influxdb.Result{Err: err}, http.StatusInternalServerError)
		return
	} else {
		w.Header().Add("X-InfluxDB-Index", fmt.Sprintf("%d", index))
	}
}

// serveMetastore returns a copy of the metastore.
func (h *Handler) serveMetastore(w http.ResponseWriter, r *http.Request) {
	// Set headers.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="meta"`)

	if err := h.server.CopyMetastore(w); err != nil {
		httpError(w, err.Error(), false, http.StatusInternalServerError)
	}
}

// serveStatus returns a set of states that the server is currently in.
func (h *Handler) serveStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("content-type", "application/json")

	pretty := r.URL.Query().Get("pretty") == "true"

	data := struct {
		Id    uint64 `json:"id"`
		Index uint64 `json:"index"`
	}{
		Id:    h.server.ID(),
		Index: h.server.Index(),
	}
	var b []byte
	if pretty {
		b, _ = json.MarshalIndent(data, "", "    ")
	} else {
		b, _ = json.Marshal(data)
	}
	w.Write(b)
}

// serveOptions returns an empty response to comply with OPTIONS pre-flight requests
func (h *Handler) serveOptions(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// servePing returns a simple response to let the client know the server is running.
func (h *Handler) servePing(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// serveIndex returns the current index of the node as the body of the response
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(fmt.Sprintf("%d", h.server.Index())))
}

// serveWait returns the current index of the node as the body of the response
// Takes optional parameters:
//     index - If specified, will poll for index before returning
//     timeout (optional) - time in milliseconds to wait until index is met before erring out
//               default timeout if not specified really big (max int64)
func (h *Handler) serveWait(w http.ResponseWriter, r *http.Request) {
	index, _ := strconv.ParseUint(r.URL.Query().Get(":index"), 10, 64)
	timeout, _ := strconv.Atoi(r.URL.Query().Get("timeout"))

	if index == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var d time.Duration
	if timeout == 0 {
		d = math.MaxInt64
	} else {
		d = time.Duration(timeout) * time.Millisecond
	}
	err := h.pollForIndex(index, d)
	if err != nil {
		w.WriteHeader(http.StatusRequestTimeout)
		return
	}
	w.Write([]byte(fmt.Sprintf("%d", h.server.Index())))
}

// pollForIndex will poll until either the index is met or it times out
// timeout is in milliseconds
func (h *Handler) pollForIndex(index uint64, timeout time.Duration) error {
	done := make(chan struct{})

	go func() {
		for {
			if h.server.Index() >= index {
				done <- struct{}{}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	for {
		select {
		case <-done:
			return nil
		case <-time.After(timeout):
			return fmt.Errorf("timed out")
		}
	}
}

// serveDataNodes returns a list of all data nodes in the cluster.
func (h *Handler) serveDataNodes(w http.ResponseWriter, r *http.Request) {
	// Generate a list of objects for encoding to the API.
	a := make([]*dataNodeJSON, 0)
	for _, n := range h.server.DataNodes() {
		a = append(a, &dataNodeJSON{
			ID:  n.ID,
			URL: n.URL.String(),
		})
	}

	w.Header().Add("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}

// serveCreateDataNode creates a new data node in the cluster.
func (h *Handler) serveCreateDataNode(w http.ResponseWriter, r *http.Request) {
	// Read in data node from request body.
	var n dataNodeJSON
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		httpError(w, err.Error(), false, http.StatusBadRequest)
		return
	}

	// Parse the URL.
	u, err := url.Parse(n.URL)
	if err != nil {
		httpError(w, "invalid data node url", false, http.StatusBadRequest)
		return
	}

	// Create the data node.
	if err := h.server.CreateDataNode(u); err == influxdb.ErrDataNodeExists {
		httpError(w, err.Error(), false, http.StatusConflict)
		return
	} else if err != nil {
		httpError(w, err.Error(), false, http.StatusInternalServerError)
		return
	}

	// Retrieve data node reference.
	node := h.server.DataNodeByURL(u)

	// Create a new replica on the broker.
	if err := h.server.Client().CreateReplica(node.ID, node.URL); err != nil {
		httpError(w, err.Error(), false, http.StatusBadGateway)
		return
	}

	// Write new node back to client.
	w.WriteHeader(http.StatusCreated)
	w.Header().Add("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(&dataNodeJSON{ID: node.ID, URL: node.URL.String()})
}

// serveDeleteDataNode removes an existing node.
func (h *Handler) serveDeleteDataNode(w http.ResponseWriter, r *http.Request) {
	// Parse node id.
	nodeID, err := strconv.ParseUint(r.URL.Query().Get(":id"), 10, 64)
	if err != nil {
		httpError(w, "invalid node id", false, http.StatusBadRequest)
		return
	}

	// Delete the node.
	if err := h.server.DeleteDataNode(nodeID); err == influxdb.ErrDataNodeNotFound {
		httpError(w, err.Error(), false, http.StatusNotFound)
		return
	} else if err != nil {
		httpError(w, err.Error(), false, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// serveProcessContinuousQueries will execute any continuous queries that should be run
func (h *Handler) serveProcessContinuousQueries(w http.ResponseWriter, r *http.Request) {
	if err := h.server.RunContinuousQueries(); err != nil {
		httpError(w, err.Error(), false, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

type dataNodeJSON struct {
	ID  uint64 `json:"id"`
	URL string `json:"url"`
}

func isAuthorizationError(err error) bool {
	_, ok := err.(influxdb.ErrAuthorize)
	return ok
}

func isMeasurementNotFoundError(err error) bool {
	return (err.Error() == "measurement not found")
}

func isFieldNotFoundError(err error) bool {
	return (strings.HasPrefix(err.Error(), "field not found"))
}

// httpResult writes a Results array to the client.
func httpResults(w http.ResponseWriter, results influxdb.Results, pretty bool) {
	if results.Error() != nil {
		if isAuthorizationError(results.Error()) {
			w.WriteHeader(http.StatusUnauthorized)
		} else if isMeasurementNotFoundError(results.Error()) {
			w.WriteHeader(http.StatusOK)
		} else if isFieldNotFoundError(results.Error()) {
			w.WriteHeader(http.StatusOK)
		} else {
			fmt.Println(results.Error())
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
	w.Header().Add("content-type", "application/json")
	var b []byte
	if pretty {
		b, _ = json.MarshalIndent(results, "", "    ")
	} else {
		b, _ = json.Marshal(results)
	}
	w.Write(b)
}

// httpError writes an error to the client in a standard format.
func httpError(w http.ResponseWriter, error string, pretty bool, code int) {
	w.Header().Add("content-type", "application/json")
	w.WriteHeader(code)

	results := influxdb.Results{Err: errors.New(error)}
	var b []byte
	if pretty {
		b, _ = json.MarshalIndent(results, "", "    ")
	} else {
		b, _ = json.Marshal(results)
	}
	w.Write(b)
}

// Filters and filter helpers

// parseCredentials returns the username and password encoded in
// a request. The credentials may be present as URL query params, or as
// a Basic Authentication header.
// as params: http://127.0.0.1/query?u=username&p=password
// as basic auth: http://username:password@127.0.0.1
func parseCredentials(r *http.Request) (string, string, error) {
	q := r.URL.Query()

	if u, p := q.Get("u"), q.Get("p"); u != "" && p != "" {
		return u, p, nil
	}
	if u, p, ok := r.BasicAuth(); ok {
		return u, p, nil
	} else {
		return "", "", fmt.Errorf("unable to parse Basic Auth credentials")
	}
}

// authenticate wraps a handler and ensures that if user credentials are passed in
// an attempt is made to authenticate that user. If authentication fails, an error is returned.
//
// There is one exception: if there are no users in the system, authentication is not required. This
// is to facilitate bootstrapping of a system with authentication enabled.
func authenticate(inner func(http.ResponseWriter, *http.Request, *influxdb.User), h *Handler, requireAuthentication bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return early if we are not authenticating
		if !requireAuthentication {
			inner(w, r, nil)
			return
		}
		var user *influxdb.User

		// TODO corylanou: never allow this in the future without users
		if requireAuthentication && h.server.UserCount() > 0 {
			username, password, err := parseCredentials(r)
			if err != nil {
				httpError(w, err.Error(), false, http.StatusUnauthorized)
				return
			}
			if username == "" {
				httpError(w, "username required", false, http.StatusUnauthorized)
				return
			}

			user, err = h.server.Authenticate(username, password)
			if err != nil {
				httpError(w, err.Error(), false, http.StatusUnauthorized)
				return
			}
		}
		inner(w, r, user)
	})
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

// determines if the client can accept compressed responses, and encodes accordingly
func gzipFilter(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			inner.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		gzw := gzipResponseWriter{Writer: gz, ResponseWriter: w}
		inner.ServeHTTP(gzw, r)
	})
}

// versionHeader taks a HTTP handler and returns a HTTP handler
// and adds the X-INFLUXBD-VERSION header to outgoing responses.
func versionHeader(inner http.Handler, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-InfluxDB-Version", version)
		inner.ServeHTTP(w, r)
	})
}

// cors responds to incoming requests and adds the appropriate cors headers
// TODO: corylanou: add the ability to configure this in our config
func cors(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set(`Access-Control-Allow-Origin`, origin)
			w.Header().Set(`Access-Control-Allow-Methods`, strings.Join([]string{
				`DELETE`,
				`GET`,
				`OPTIONS`,
				`POST`,
				`PUT`,
			}, ", "))

			w.Header().Set(`Access-Control-Allow-Headers`, strings.Join([]string{
				`Accept`,
				`Accept-Encoding`,
				`Authorization`,
				`Content-Length`,
				`Content-Type`,
				`X-CSRF-Token`,
				`X-HTTP-Method-Override`,
			}, ", "))
		}

		if r.Method == "OPTIONS" {
			return
		}

		inner.ServeHTTP(w, r)
	})
}

func requestID(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := uuid.NewUUID()
		r.Header.Set("Request-Id", uid.String())
		w.Header().Set("Request-Id", r.Header.Get("Request-Id"))

		inner.ServeHTTP(w, r)
	})
}

func logging(inner http.Handler, name string, weblog *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		l := &responseLogger{w: w}
		inner.ServeHTTP(l, r)
		logLine := buildLogLine(l, r, start)
		weblog.Println(logLine)
	})
}

func recovery(inner http.Handler, name string, weblog *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		l := &responseLogger{w: w}
		inner.ServeHTTP(l, r)
		if err := recover(); err != nil {
			logLine := buildLogLine(l, r, start)
			logLine = fmt.Sprintf(`%s [err:%s]`, logLine, err)
			weblog.Println(logLine)
		}
	})
}
