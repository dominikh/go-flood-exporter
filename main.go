package main

// This is very crude and hacky code. Take it or leave it.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/cookiejar"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var errTooLong = errors.New("line too long")

func sse(r *bufio.Reader) (event string, data []byte, err error) {
	for {
		line, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", nil, err
		}
		if isPrefix {
			return "", nil, errTooLong
		}
		if len(line) == 0 {
			return event, data, err
		}
		if line[0] == ':' {
			// line is a comment
			continue
		}
		parts := bytes.SplitN(line, []byte(":"), 2)
		field := string(parts[0])
		var payload []byte
		if len(parts) == 2 {
			payload = parts[1]
		}
		switch field {
		case "event":
			event = string(payload)
		case "data":
			// We don't support multiple consecutive data lines, Flood doesn't use them.
			data = payload
		}
	}
}

type Collector struct {
	client     *Client
	trackerMap map[string]string
}

type torrent struct {
	DownTotal   uint64   `json:"downTotal"`
	UpTotal     uint64   `json:"upTotal"`
	SizeBytes   uint64   `json:"sizeBytes"`
	TrackerURIs []string `json:"trackerURIs"`
	Status      []string `json:"status"`
}

func tracker(mapping map[string]string, uris []string) (string, bool) {
	for _, uri := range uris {
		if t, ok := mapping[uri]; ok {
			return t, true
		}
	}
	return "", false
}
func NewCollector(address, username, password string, trackerMap map[string]string) *Collector {
	return &Collector{
		client:     NewClient(address, username, password),
		trackerMap: trackerMap,
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	var (
		total      = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_total"}, []string{"tracker"})
		statuses   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_by_status"}, []string{"tracker", "status"})
		activity   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_by_activity"}, []string{"tracker", "status"})
		completion = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_by_completion"}, []string{"tracker", "status"})
		errors     = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_by_error"}, []string{"tracker", "status"})
		size       = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_size_bytes"}, []string{"tracker"})
		in         = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "flood_in_bytes"}, []string{"tracker"})
		out        = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "flood_out_bytes"}, []string{"tracker"})
	)
	total.Describe(ch)
	statuses.Describe(ch)
	activity.Describe(ch)
	completion.Describe(ch)
	errors.Describe(ch)
	size.Describe(ch)
	in.Describe(ch)
	out.Describe(ch)
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	torrents, err := c.client.Fetch()
	if err != nil {
		ch <- prometheus.NewInvalidMetric(prometheus.NewInvalidDesc(err), err)
		return
	}

	var (
		total      = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_total"}, []string{"tracker"})
		statuses   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_by_status"}, []string{"tracker", "status"})
		activity   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_by_activity"}, []string{"tracker", "status"})
		completion = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_by_completion"}, []string{"tracker", "status"})
		errors     = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_by_error"}, []string{"tracker", "status"})
		size       = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flood_torrents_size_bytes"}, []string{"tracker"})
		in         = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "flood_in_bytes"}, []string{"tracker"})
		out        = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "flood_out_bytes"}, []string{"tracker"})
	)

	for _, t := range c.trackerMap {
		total.WithLabelValues(t).Set(0)
		size.WithLabelValues(t).Set(0)
		in.WithLabelValues(t).Add(0)
		out.WithLabelValues(t).Add(0)

		for _, status := range []string{"checking", "downloading", "seeding", "stopped"} {
			statuses.WithLabelValues(t, status).Set(0)
		}

		activity.WithLabelValues(t, "active").Set(0)
		activity.WithLabelValues(t, "inactive").Set(0)

		completion.WithLabelValues(t, "complete").Set(0)
		completion.WithLabelValues(t, "incomplete").Set(0)

		errors.WithLabelValues(t, "error").Set(0)
		errors.WithLabelValues(t, "no error").Set(0)
	}

	for _, torr := range torrents {
		t, _ := tracker(c.trackerMap, torr.TrackerURIs)
		total.WithLabelValues(t).Inc()

		complete := false
		hasError := false
		for _, status := range torr.Status {
			switch status {
			case "active", "inactive":
				activity.WithLabelValues(t, status).Inc()
			case "checking", "downloading", "seeding", "stopped":
				statuses.WithLabelValues(t, status).Inc()
			case "complete":
				complete = true
			case "error":
				hasError = true
			}
		}

		if complete {
			completion.WithLabelValues(t, "complete").Inc()
		} else {
			completion.WithLabelValues(t, "incomplete").Inc()
		}
		if hasError {
			errors.WithLabelValues(t, "error").Inc()
		} else {
			errors.WithLabelValues(t, "no error").Inc()
		}

		in.WithLabelValues(t).Add(float64(torr.DownTotal))
		out.WithLabelValues(t).Add(float64(torr.UpTotal))
		size.WithLabelValues(t).Add(float64(torr.SizeBytes))
	}

	total.Collect(ch)
	statuses.Collect(ch)
	activity.Collect(ch)
	completion.Collect(ch)
	errors.Collect(ch)
	size.Collect(ch)
	in.Collect(ch)
	out.Collect(ch)
}

type Client struct {
	address  string
	username string
	password string
	client   *http.Client
}

func NewClient(address, username, password string) *Client {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
	}
	return &Client{
		address:  address,
		username: username,
		password: password,
		client:   client,
	}
}

func (c *Client) Fetch() (map[string]torrent, error) {
	payload := struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{c.username, c.password}
	// OPT this payload is static, so why create garbage? But who cares…
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	body := bytes.NewReader(b)
	resp, err := c.client.Post(c.address+"/api/auth/authenticate", "application/json", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if cat := resp.StatusCode / 100; cat == 4 || cat == 5 {
		return nil, errors.New("unexpected status code")
	}
	resp, err = c.client.Get(c.address + "/api/activity-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// OPT this buffer is absurdly big to work around our poor approach to parsing server-sent events. For our use-case
	// it doesn't matter; this function gets called once every 15 seconds, the program is doing nothing else, and it is
	// running on an oversized server.
	br := bufio.NewReaderSize(resp.Body, 1024*1024*5)
	for {
		event, data, err := sse(br)
		if err == io.EOF {
			return nil, errors.New("protocol error")
		}
		if err == errTooLong {
			// we don't care about these, for now
			continue
		}
		if err != nil {
			return nil, err
		}
		if event == "TORRENT_LIST_FULL_UPDATE" {
			torrents := map[string]torrent{}
			if err := json.Unmarshal(data, &torrents); err != nil {
				return nil, err
			}
			return torrents, nil
		}
	}
}

var (
	telemetryAddr = flag.String("telemetry.addr", ":4100", "address for Flood exporter")
	metricsPath   = flag.String("telemetry.path", "/metrics", "URL path for surfacing collected metrics")

	floodRPC        = flag.String("flood.rpc", "", "URL of Flood instance")
	floodUser       = flag.String("flood.user", "", "Flood username")
	floodPass       = flag.String("flood.pass", "", "Flood password")
	floodPassFile   = flag.String("flood.pass-file", "", "File to read Flood password from")
	floodTrackerMap = flag.String("flood.tracker-map", "", "A mapping of tracker domains to tracker names, in the format 'example.com=EX foobar.com=FOO'")
)

func main() {
	log.SetFlags(0)
	flag.Parse()

	if *floodPass != "" && *floodPassFile != "" {
		log.Fatal("shouldn't specify both -flood.pass and -flood.pass-file")
	}

	pass := *floodPass
	if *floodPassFile != "" {
		b, err := ioutil.ReadFile(*floodPassFile)
		if err != nil {
			log.Fatalf("couldn't read password: %s", err)
		}
		pass = string(bytes.TrimRight(b, "\n"))
	}

	trackerMap := map[string]string{}
	for _, m := range strings.Fields(*floodTrackerMap) {
		p := strings.SplitN(m, "=", 2)
		trackerMap[p[0]] = p[1]
	}

	prometheus.MustRegister(NewCollector(*floodRPC, *floodUser, pass, trackerMap))
	http.Handle(*metricsPath, promhttp.Handler())
	if err := http.ListenAndServe(*telemetryAddr, nil); err != nil {
		log.Fatal(err)
	}
}
