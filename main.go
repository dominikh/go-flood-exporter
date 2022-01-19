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

type transferSummary struct {
	UpTotal   uint64 `json:"upTotal"`
	DownTotal uint64 `json:"downTotal"`
}

type Status struct {
	Checking    int `json:"checking"`
	Seeding     int `json:"seeding"`
	Complete    int `json:"complete"`
	Downloading int `json:"downloading"`
	Stopped     int `json:"stopped"`
	Error       int `json:"error"`
	Inactive    int `json:"inactive"`
	Active      int `json:"active"`
}

type taxonomy struct {
	StatusCounts Status `json:"statusCounts"`
}

type Collector struct {
	client *Client

	byActivity   *prometheus.Desc
	byCompletion *prometheus.Desc
	byStatus     *prometheus.Desc
	byError      *prometheus.Desc
	in           *prometheus.Desc
	out          *prometheus.Desc
}

func NewCollector(address, username, password string) *Collector {
	return &Collector{
		client: NewClient(address, username, password),

		byActivity:   prometheus.NewDesc("flood_torrents_by_activity", "", []string{"status"}, nil),
		byCompletion: prometheus.NewDesc("flood_torrents_by_completion", "", []string{"status"}, nil),
		byStatus:     prometheus.NewDesc("flood_torrents_by_status", "", []string{"status"}, nil),
		byError:      prometheus.NewDesc("flood_torrents_by_error", "", []string{"status"}, nil),
		in:           prometheus.NewDesc("flood_in_bytes", "", nil, nil),
		out:          prometheus.NewDesc("flood_out_bytes", "", nil, nil),
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.byActivity
	ch <- c.byCompletion
	ch <- c.byStatus
	ch <- c.byError
	ch <- c.in
	ch <- c.out
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	status, traffic, err := c.client.Fetch()
	if err != nil {
		ch <- prometheus.NewInvalidMetric(prometheus.NewInvalidDesc(err), err)
		return
	}

	total := status.Active + status.Inactive
	ch <- prometheus.MustNewConstMetric(c.byActivity, prometheus.GaugeValue, float64(status.Active), "active")
	ch <- prometheus.MustNewConstMetric(c.byActivity, prometheus.GaugeValue, float64(status.Inactive), "inactive")

	ch <- prometheus.MustNewConstMetric(c.byCompletion, prometheus.GaugeValue, float64(status.Complete), "complete")
	ch <- prometheus.MustNewConstMetric(c.byCompletion, prometheus.GaugeValue, float64(total-status.Complete), "incomplete")

	ch <- prometheus.MustNewConstMetric(c.byStatus, prometheus.GaugeValue, float64(status.Checking), "checking")
	ch <- prometheus.MustNewConstMetric(c.byStatus, prometheus.GaugeValue, float64(status.Seeding), "seeding")
	ch <- prometheus.MustNewConstMetric(c.byStatus, prometheus.GaugeValue, float64(status.Downloading), "downloading")
	ch <- prometheus.MustNewConstMetric(c.byStatus, prometheus.GaugeValue, float64(status.Stopped), "stopped")

	ch <- prometheus.MustNewConstMetric(c.byError, prometheus.GaugeValue, float64(status.Error), "error")
	ch <- prometheus.MustNewConstMetric(c.byError, prometheus.GaugeValue, float64(total-status.Error), "no error")

	ch <- prometheus.MustNewConstMetric(c.in, prometheus.CounterValue, float64(traffic.DownTotal))
	ch <- prometheus.MustNewConstMetric(c.out, prometheus.CounterValue, float64(traffic.UpTotal))
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

func (c *Client) Fetch() (*Status, *transferSummary, error) {
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
		return nil, nil, err
	}
	defer resp.Body.Close()
	if cat := resp.StatusCode / 100; cat == 4 || cat == 5 {
		return nil, nil, errors.New("unexpected status code")
	}
	resp, err = c.client.Get(c.address + "/api/activity-stream")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	var traffic *transferSummary
	var status *taxonomy
	for {
		event, data, err := sse(br)
		if err == io.EOF {
			return nil, nil, errors.New("protocol error")
		}
		if err == errTooLong {
			// we don't care about these, for now
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		switch event {
		case "TRANSFER_SUMMARY_FULL_UPDATE":
			if err := json.Unmarshal(data, &traffic); err != nil {
				return nil, nil, err
			}
		case "TAXONOMY_FULL_UPDATE":
			if err := json.Unmarshal(data, &status); err != nil {
				return nil, nil, err
			}
		}
		if traffic != nil && status != nil {
			// We got all fields we care about
			return &status.StatusCounts, traffic, nil
		}
	}
}

var (
	telemetryAddr = flag.String("telemetry.addr", ":4100", "address for Flood exporter")
	metricsPath   = flag.String("telemetry.path", "/metrics", "URL path for surfacing collected metrics")

	floodRPC      = flag.String("flood.rpc", "", "URL of Flood instance")
	floodUser     = flag.String("flood.user", "", "Flood username")
	floodPass     = flag.String("flood.pass", "", "Flood password")
	floodPassFile = flag.String("flood.pass-file", "", "File to read Flood password from")
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

	prometheus.MustRegister(NewCollector(*floodRPC, *floodUser, pass))
	http.Handle(*metricsPath, promhttp.Handler())
	if err := http.ListenAndServe(*telemetryAddr, nil); err != nil {
		log.Fatal(err)
	}
}
