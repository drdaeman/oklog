package store

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	level "github.com/go-kit/kit/log/experimental_level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/oklog/oklog/pkg/cluster"
	"github.com/oklog/oklog/pkg/stream"
)

// These are the store API URL paths.
const (
	APIPathUserQuery      = "/query"
	APIPathInternalQuery  = "/_query"
	APIPathUserStream     = "/stream"
	APIPathInternalStream = "/_stream"
	APIPathReplicate      = "/replicate"
	APIPathClusterState   = "/_clusterstate"
)

// API serves the store API.
type API struct {
	peer               *cluster.Peer
	log                Log
	client             *http.Client
	replicatedSegments prometheus.Counter
	replicatedBytes    prometheus.Counter
	duration           *prometheus.HistogramVec
	logger             log.Logger
}

// NewAPI returns a usable API.
func NewAPI(
	peer *cluster.Peer,
	log Log,
	client *http.Client,
	replicatedSegments, replicatedBytes prometheus.Counter,
	duration *prometheus.HistogramVec,
	logger log.Logger,
) *API {
	return &API{
		peer:               peer,
		log:                log,
		client:             client,
		replicatedSegments: replicatedSegments,
		replicatedBytes:    replicatedBytes,
		duration:           duration,
		logger:             logger,
	}
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	iw := &interceptingWriter{http.StatusOK, w}
	w = iw
	defer func(begin time.Time) {
		a.duration.WithLabelValues(
			r.Method,
			r.URL.Path,
			strconv.Itoa(iw.code),
		).Observe(time.Since(begin).Seconds())
	}(time.Now())

	method, path := r.Method, r.URL.Path
	switch {
	case method == "GET" && path == "/":
		r.URL.Path = APIPathUserQuery
		http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
	case method == "GET" && path == APIPathUserQuery:
		a.handleUserQuery(w, r, false)
	case method == "HEAD" && path == APIPathUserQuery:
		a.handleUserQuery(w, r, true)
	case method == "GET" && path == APIPathInternalQuery:
		a.handleInternalQuery(w, r, false)
	case method == "HEAD" && path == APIPathInternalQuery:
		a.handleInternalQuery(w, r, true)
	case method == "GET" && path == APIPathUserStream:
		a.handleUserStream(w, r)
	case method == "GET" && path == APIPathInternalStream:
		a.handleInternalStream(w, r)
	case method == "POST" && path == APIPathReplicate:
		a.handleReplicate(w, r)
	case method == "GET" && path == APIPathClusterState:
		a.handleClusterState(w, r)
	default:
		http.NotFound(w, r)
	}
}

type interceptingWriter struct {
	code int
	http.ResponseWriter
}

func (iw *interceptingWriter) WriteHeader(code int) {
	iw.code = code
	iw.ResponseWriter.WriteHeader(code)
}

func (a *API) handleUserQuery(w http.ResponseWriter, r *http.Request, statsOnly bool) {
	begin := time.Now()

	members := a.peer.Current(cluster.PeerTypeStore)
	if len(members) <= 0 {
		// Very odd; we should at least find ourselves!
		http.Error(w, "no store nodes available", http.StatusServiceUnavailable)
		return
	}

	query, err := MakeQueryParams(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	method := "GET"
	if statsOnly {
		method = "HEAD"
	}

	var requests []*http.Request
	for _, hostport := range members {
		u, err := url.Parse(fmt.Sprintf("http://%s/store%s", hostport, APIPathInternalQuery))
		if err != nil {
			err = errors.Wrapf(err, "constructing URL for %s", hostport)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		query.EncodeTo(u.Query()) // use query directly, no translation needed
		req, err := http.NewRequest(method, u.String(), nil)
		if err != nil {
			err = errors.Wrapf(err, "constructing request for %s", hostport)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		requests = append(requests, req)
	}

	type response struct {
		resp *http.Response
		err  error
	}
	c := make(chan response, len(requests))
	for _, req := range requests {
		go func(req *http.Request) {
			// TODO(pb): don't use http.DefaultClient
			resp, err := http.DefaultClient.Do(req)
			c <- response{resp, err}
		}(req)
	}

	responses := make([]response, cap(c))
	for i := 0; i < cap(c); i++ {
		responses[i] = <-c
	}
	result := QueryResult{
		Params: query,
	}
	for _, response := range responses {
		if response.err != nil {
			level.Error(a.logger).Log("during", "query_gather", "err", response.err)
			result.ErrorCount++
			continue
		}
		if response.resp.StatusCode != http.StatusOK {
			buf, err := ioutil.ReadAll(response.resp.Body)
			if err != nil {
				buf = []byte(err.Error())
			}
			if len(buf) == 0 {
				buf = []byte("unknown")
			}
			response.resp.Body.Close()
			level.Error(a.logger).Log("during", "query_gather", "status_code", response.resp.StatusCode, "err", strings.TrimSpace(string(buf)))
			result.ErrorCount++
			continue
		}
		var partialResult QueryResult
		partialResult.DecodeFrom(response.resp)
		if err := result.Merge(partialResult); err != nil {
			err = errors.Wrap(err, "merging results")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	result.Duration = time.Since(begin).String()
	result.EncodeTo(w)
}

func (a *API) handleInternalQuery(w http.ResponseWriter, r *http.Request, statsOnly bool) {
	query, err := MakeQueryParams(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	result, err := a.log.Query(query, statsOnly)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	result.EncodeTo(w)
}

func (a *API) handleUserStream(w http.ResponseWriter, r *http.Request) {
	query, err := MakeQueryParams(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "can't stream to your client", http.StatusPreconditionFailed)
		return
	}

	peerFactory := func() []string {
		return a.peer.Current(cluster.PeerTypeStore)
	}

	readerFactory := stream.HTTPReaderFactory(a.client, func(addr string) string {
		u, err := url.Parse(fmt.Sprintf("http://%s/store%s", addr, APIPathInternalStream))
		if err != nil {
			panic(err)
		}
		query.EncodeTo(u.Query())
		return u.String()
	})

	records := make(chan []byte)
	go stream.Execute(
		r.Context(),
		peerFactory,
		readerFactory,
		records,
		time.Sleep,
		time.NewTicker,
	)

	for {
		select {
		case record := <-records:
			w.Write(append(record, '\n'))
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (a *API) handleInternalStream(w http.ResponseWriter, r *http.Request) {
	query, err := MakeQueryParams(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "can't stream to your client", http.StatusPreconditionFailed)
		return
	}

	records := a.log.Stream(r.Context(), query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return // the cancelation is transitive, just need to return

		case record := <-records:
			fmt.Fprintf(w, "%s\n", record)
			flusher.Flush()
		}
	}
}

func (a *API) handleReplicate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	segment, err := a.log.Create()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	low, high, n, err := mergeRecords(segment, r.Body)
	if err != nil {
		segment.Delete()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if n == 0 {
		segment.Delete()
		fmt.Fprintln(w, "No records")
		return
	}
	if err := segment.Close(low, high); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.replicatedSegments.Inc()
	a.replicatedBytes.Add(float64(n))
	fmt.Fprintln(w, "OK")
}

func (a *API) handleClusterState(w http.ResponseWriter, r *http.Request) {
	buf, err := json.MarshalIndent(a.peer.State(), "", "    ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(buf)
}
